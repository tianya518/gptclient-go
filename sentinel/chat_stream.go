package sentinel

// chat_stream.go —— 主 SSE 流处理：POST /backend-api/f/conversation 的读取循环，
// 以及 SSE 结束后的分发（生图轮询 / 直接定稿 / WS catchup）。
// 另含 conversation_id / turn_exchange_id 的追踪助手（stream 与 ws 通道共用）。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gorilla/websocket"
)

// streamConversation 发 f/conversation，解析 stream_handoff 后走 WebSocket 续流
func (c *Client) streamConversation(body interface{}, opts ChatOptions, sentinelToken, proofToken, conduitToken, turnTraceID string, wsConn *websocket.Conn, handler StreamHandler) (*ChatResult, error) {
	headers := map[string]string{
		"Accept":       "text/event-stream",
		"Content-Type": "application/json",
		"openai-sentinel-chat-requirements-token": sentinelToken,
		"x-conduit-token":                         conduitToken,
		"x-oai-turn-trace-id":                     turnTraceID,
		"x-openai-target-path":                    "/backend-api/f/conversation",
		"x-openai-target-route":                   "/backend-api/f/conversation",
	}
	if proofToken != "" {
		headers["openai-sentinel-proof-token"] = proofToken
	}

	resp, err := c.httpClient.R().
		SetHeaders(headers).
		SetBody(body).
		DisableAutoReadResponse().
		Post("/backend-api/f/conversation")
	if err != nil {
		return nil, fmt.Errorf("conversation request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		// 422 时保留更长 body，便于看全 pydantic union 校验细节
		limit := 500
		if resp.StatusCode == 422 {
			limit = 4000
		}
		return nil, fmt.Errorf("conversation %d: %s", resp.StatusCode, truncateStr(string(b), limit))
	}

	result := &ChatResult{
		userReferenceFileIDs: make(map[string]bool),
		pictureV2Path:        opts.ForcePictureV2,
	}
	if opts.ForcePictureV2 {
		result.ExpectGeneratedImages = true
	}
	for _, f := range opts.Images {
		if f.FileID != "" {
			result.userReferenceFileIDs[f.FileID] = true
		}
	}
	var lastText string
	var useDeltaEncoding bool
	var currentEvent string
	var handoffTopicID string

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimSpace(line[7:])
			if currentEvent == "delta_encoding" {
				useDeltaEncoding = true
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		payload := strings.TrimSpace(line[6:])
		if payload == "" || payload == "[DONE]" || payload == `"v1"` {
			continue
		}

		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			currentEvent = ""
			continue
		}

		if c.StreamRecorder != nil {
			c.StreamRecorder.RecordSSE(currentEvent, payload, evt)
		}
		result.ArtifactSignals = MergeSignals(result.ArtifactSignals, ExtractSignalsFromJSON(evt))
		c.MergeApplyAndEmitArtifacts(result, opts)
		if tid := findTurnExchangeID(evt); tid != "" {
			handoffTopicID = "conversation-turn-" + tid
			result.TurnExchangeID = tid
		}

		c.probeImageRouteFromSSE(payload, result, opts)
		if strings.Contains(payload, "dalle") || strings.Contains(payload, `"tool"`) || strings.Contains(payload, "image") || strings.Contains(payload, "thought") || strings.Contains(payload, "reasoning_content") {
			c.logf("[debug-sse] payload: %s", payload)
		}

		c.noteConversationID(result, opts, evt)

		evtType, _ := evt["type"].(string)
		switch evtType {
		case "resume_conversation_token":
			currentEvent = ""
			continue
		case "stream_handoff":
			_, topicID := parseStreamHandoff(evt)
			if topicID != "" {
				handoffTopicID = topicID
			}
			currentEvent = ""
			continue
		case "server_ste_metadata":
			if handoffTopicID == "" {
				if md, ok := evt["metadata"].(map[string]interface{}); ok {
					if tid, ok := md["turn_exchange_id"].(string); ok && tid != "" {
						handoffTopicID = "conversation-turn-" + tid
					}
				}
			}
			currentEvent = ""
			continue
		}

		// 兼容 event: server_ste_metadata + data 内无 type 字段的旧格式
		if currentEvent == "server_ste_metadata" && handoffTopicID == "" {
			if tid, ok := evt["turn_exchange_id"].(string); ok && tid != "" {
				handoffTopicID = "conversation-turn-" + tid
			} else if md, ok := evt["metadata"].(map[string]interface{}); ok {
				if tid, ok := md["turn_exchange_id"].(string); ok && tid != "" {
					handoffTopicID = "conversation-turn-" + tid
				}
			}
		}

		checkImageTaskID(evt, result)
		c.MergeApplyAndEmitArtifacts(result, opts)
		if useDeltaEncoding && currentEvent == "delta" {
			c.processDeltaSSE(evt, result, opts, &lastText, handler)
		} else {
			c.processFullSSE(evt, result, opts, &lastText, handler)
		}
		currentEvent = ""
	}
	if err := scanner.Err(); err != nil {
		// 流被中途截断（网络抖动等）：已解析到的内容仍走后续分发，仅记录告警。
		c.logf("[sse] 读取事件流中断: %v", err)
	}

	c.MergeApplyAndEmitArtifacts(result, opts)
	// 仅当 final 通道正文已完整时才跳过 WS catchup（避免未出 JSON 就 handoff）
	result.bodyStreamFromSSE = result.assistantFinalText != ""

	if handoffTopicID == "" && result.TurnExchangeID != "" {
		handoffTopicID = "conversation-turn-" + result.TurnExchangeID
		c.logf("[handoff] turn topic 来自 TurnExchangeID: %s", handoffTopicID)
	}

	// ── SSE 结束后的分发：生图轮询 / 直接定稿 / WS 续流 ─────────────────────────
	// 三条互斥路径由 resolveImageHandoff（是否生图）与 CanSkipImageWSAfterSSE（是否已收齐）决定：
	//   1) 生图且 SSE 已收齐（单图常见）  → 直接定稿，无需轮询/WS
	//   2) 生图但未收齐                    → 关 WS，转 GET /conversation 轮询收齐 N 张
	//   3) 非生图                          → WS catchup 补全正文（thinking 文本轮次）
	needImageHandoff := c.resolveImageHandoff(result, opts, handoffTopicID)
	switch {
	case needImageHandoff && result.CanSkipImageWSAfterSSE():
		c.logf("[handoff] SSE 阶段已收齐出图 slots=%d file_ids=%v，直接定稿", len(result.imageSlots), result.ImageFileIDs)
		if wsConn != nil {
			_ = wsConn.Close()
		}
		c.FinishImageGenWS(result, opts)

	case needImageHandoff && result.ConversationID != "":
		c.collectImagesViaPolling(wsConn, result, opts,
			fmt.Sprintf("[handoff] 生图转 conversation 轮询 sawTool=%v ImageTaskID=%q ForcePictureV2=%v conv=%s",
				result.sawImageGenTool, result.ImageTaskID, opts.ForcePictureV2, result.ConversationID))

	case result.asyncTextResolved:
		// thinking 文本轮次（含带附件走 async 的场景）：探测阶段已从 conversation 取到最终正文，
		// 无需 WS catchup；直接关闭预建 WS，避免空闲 WS 失效。
		c.logf("[handoff] thinking 文本轮次：探测已取到最终正文，跳过 WS catchup conv=%s", result.ConversationID)
		if wsConn != nil {
			_ = wsConn.Close()
		}

	case handoffTopicID != "" && wsConn != nil:
		// 非生图轮次：正文可能仍需从 WS catchup 补全（subscribeWSStream 内部会在 bodyStreamFromSSE 时跳过）。
		c.logf("[handoff] 订阅 WebSocket topic: %s", handoffTopicID)
		if err := c.subscribeWSStream(wsConn, handoffTopicID, result, opts, &lastText, handler); err != nil {
			// 代理/本机掐断 WS（1006 / unexpected EOF）时，官网会话往往已出完整正文。
			// 先降级 GET /conversation 拉最终答复，避免把成功轮次误报成 API 流错误。
			if !c.recoverFinalTextAfterStreamFailure(result, &lastText, handler, err) {
				return nil, fmt.Errorf("ws stream: %w", err)
			}
		}
		c.MergeApplyAndEmitArtifacts(result, opts)
		// 安全网：若 WS 期间才识别出生图（batch_requests / 图像工具）但尚未收齐图片，
		// 补一轮 conversation 轮询把 N 张图收全（thinking 批量路径图片经轮询下发）。
		if !c.DisableAutoImage && result.sawImageGenTool && result.ConversationID != "" &&
			!result.HasDalleGeneratedOutput() {
			c.collectImagesViaPolling(nil, result, opts,
				fmt.Sprintf("[handoff] WS 结束后补轮询收图 conv=%s", result.ConversationID))
		}
	}

	// 生图成功且有 DALL·E 产出时清除排队提示文字
	if result.ExpectGeneratedImages && result.HasDalleGeneratedOutput() {
		lastText = ""
		result.assistantFinalText = ""
	}
	if result.assistantFinalText != "" {
		result.Text = result.assistantFinalText
	} else {
		result.Text = lastText
	}
	return result, nil
}

// findTurnExchangeID 递归查找事件中的 turn_exchange_id / working_turn_id（用于拼 WS turn topic）。
func findTurnExchangeID(v interface{}) string {
	switch x := v.(type) {
	case map[string]interface{}:
		if tid, ok := x["turn_exchange_id"].(string); ok && tid != "" {
			return tid
		}
		if tid, ok := x["working_turn_id"].(string); ok && tid != "" {
			return tid
		}
		for _, val := range x {
			if id := findTurnExchangeID(val); id != "" {
				return id
			}
		}
	case []interface{}:
		for _, item := range x {
			if id := findTurnExchangeID(item); id != "" {
				return id
			}
		}
	}
	return ""
}

func (result *ChatResult) noteTurnExchangeFromMessage(msg map[string]interface{}) {
	if result == nil || msg == nil {
		return
	}
	meta, ok := msg["metadata"].(map[string]interface{})
	if !ok {
		return
	}
	if tid, ok := meta["turn_exchange_id"].(string); ok && tid != "" {
		result.TurnExchangeID = tid
		return
	}
	if tid, ok := meta["working_turn_id"].(string); ok && tid != "" {
		result.TurnExchangeID = tid
	}
}

func (c *Client) noteConversationID(result *ChatResult, opts ChatOptions, evt map[string]interface{}) {
	if result == nil || evt == nil {
		return
	}
	cid, ok := evt["conversation_id"].(string)
	if !ok || cid == "" {
		return
	}
	prev := result.ConversationID
	result.ConversationID = cid
	if prev != cid && opts.OnConversationID != nil {
		opts.OnConversationID(cid)
	}
}
