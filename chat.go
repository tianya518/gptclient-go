package sentinel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ImageAspectRatio 图片宽高比
type ImageAspectRatio string

const (
	ImageAspectAuto      ImageAspectRatio = ""      // 自动（默认）
	ImageAspectSquare    ImageAspectRatio = "1:1"   // 方形
	ImageAspectPortrait  ImageAspectRatio = "3:4"   // 竖版
	ImageAspectStory     ImageAspectRatio = "9:16"  // 故事版
	ImageAspectLandscape ImageAspectRatio = "4:3"   // 横版
	ImageAspectWidescreen ImageAspectRatio = "16:9" // 宽屏
)

// ChatOptions 对话请求参数
type ChatOptions struct {
	Text           string
	Images         []UploadedFile
	ForcePictureV2 bool
	// ImageAspect 仅在 ForcePictureV2=true 时生效，指定生成图片的宽高比
	ImageAspect ImageAspectRatio
	// Artifacts 产物（生图/沙箱文件）流式侧信道；正文仍走 StreamHandler
	Artifacts ArtifactStreamConfig
	// OnConversationID 首次得知 conversation_id 时回调（server 用于提前 Register，以便流中即可拉取 /api/image/proxy）
	OnConversationID func(convID string)
}

// Chat 发送一轮对话，返回完整结果（非流式）
func (c *Client) Chat(opts ChatOptions) (*ChatResult, error) {
	return c.ChatStream(opts, nil)
}

// ChatStream 发送一轮对话，通过 handler 回调实时接收增量文本
func (c *Client) ChatStream(opts ChatOptions, handler StreamHandler) (*ChatResult, error) {
	turnTraceID := GenerateUUID()

	c.logf("[step 1] 获取 conduit token...")
	conduitToken, err := c.getConduitToken(c.model, turnTraceID, runeSlice(opts.Text, 5))
	if err != nil {
		return nil, fmt.Errorf("get conduit token: %w", err)
	}

	c.logf("[step 2] 获取 sentinel token...")
	sentinelToken, proofToken, err := c.getSentinelToken()
	if err != nil {
		return nil, fmt.Errorf("get sentinel token: %w", err)
	}

	c.logf("[step 2.5] 建立 WebSocket 连接...")
	wsConn, err := c.dialChatWS()
	if err != nil {
		return nil, fmt.Errorf("dial ws: %w", err)
	}
	defer wsConn.Close()

	promptText := opts.Text
	if opts.ForcePictureV2 && opts.ImageAspect != ImageAspectAuto {
		promptText += "\n\n将宽高比设为 " + string(opts.ImageAspect)
	}

	// 区分图片（multimodal）和文档（my_files）
	// 图片需要插入 content.parts 作为 image_asset_pointer；文档只放 metadata.attachments
	var parts []interface{}
	hasImages := false
	for _, f := range opts.Images {
		if f.UseCase == "multimodal" {
			parts = append(parts, f.ToAssetPointerPart())
			hasImages = true
		}
	}
	parts = append(parts, promptText)

	contentType := "text"
	if hasImages {
		contentType = "multimodal_text"
	}

	attachments := []Attachment{}
	for _, f := range opts.Images {
		attachments = append(attachments, f.ToAttachment())
	}

	msgID := GenerateUUID()
	userMsgObj := map[string]interface{}{
		"id":          msgID,
		"author":      map[string]string{"role": "user"},
		"create_time": float64(time.Now().UnixMilli()) / 1000.0,
		"content": map[string]interface{}{
			"content_type": contentType,
			"parts":        parts,
		},
		"metadata": map[string]interface{}{
			"developer_mode_connector_ids": []string{},
			"selected_sources":             []string{},
			"selected_github_repos":        []string{},
			"selected_all_github_repos":    false,
			"serialization_metadata":       map[string]interface{}{"custom_symbol_offsets": []interface{}{}},
		},
	}
	if len(attachments) > 0 {
		userMsgObj["metadata"].(map[string]interface{})["attachments"] = attachments
	}

	systemHints := []string{}
	if opts.ForcePictureV2 {
		systemHints = append(systemHints, "picture_v2")
		meta := userMsgObj["metadata"].(map[string]interface{})
		meta["system_hints"] = systemHints
		// picture_v2 不能带 selected_sources，否则直接失败 (静默失败)
		delete(meta, "selected_sources")
	}

	body := map[string]interface{}{
		"action": "next",
		"messages": []map[string]interface{}{
			userMsgObj,
		},
		"parent_message_id":        c.parentMessageID,
		"model":                    c.model,
		"timezone_offset_min":      -480,
		"timezone":                 "Asia/Shanghai",
		"conversation_mode":        map[string]string{"kind": "primary_assistant"},
		"enable_message_followups": true,
		"system_hints":             systemHints,
		"supports_buffering":       true,
		"supported_encodings":      []string{"v1"},
		"client_contextual_info": map[string]interface{}{
			"is_dark_mode":      false,
			"time_since_loaded": int(math.Round(perfNowMs(c.startTime) / 1000.0)),
			"page_height":       1014,
			"page_width":        1055,
			"pixel_ratio":       1,
			"screen_height":     1080,
			"screen_width":      1920,
			"app_name":          "chatgpt.com",
		},
		"history_and_training_disabled":        c.tempMode,
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"thinking_effort":                      "standard",
	}
	if c.conversationID != "" {
		body["conversation_id"] = c.conversationID
	}

	convDesc := c.conversationID
	if convDesc == "" {
		convDesc = "(新对话)"
	}
	c.logf("[step 3] 发送对话: model=%s, conversation=%s, turn=%d", c.model, convDesc, c.turnCount+1)

	result, err := c.streamConversation(body, opts, sentinelToken, proofToken, conduitToken, turnTraceID, wsConn, handler)
	if err != nil {
		return nil, err
	}

	if result.ConversationID != "" {
		c.conversationID = result.ConversationID
	}
	if result.LastAssistantMsgID != "" {
		c.parentMessageID = result.LastAssistantMsgID
	}
	c.turnCount++

	c.logf("[info] conversation_id=%s, turn=%d, reply=%d字, thinking=%d字, final=%d字",
		c.conversationID, c.turnCount,
		len([]rune(result.Text)),
		len([]rune(result.ThinkingText)),
		len([]rune(result.assistantFinalText)))
	LogContentPreview(c.logf, "reply-text", result.Text)
	if result.ThinkingText != "" && result.ThinkingText != result.Text {
		LogContentPreview(c.logf, "reply-thinking", result.ThinkingText)
	}

	c.MergeApplyAndEmitArtifacts(result, opts)
	if len(result.SandboxArtifacts) > 0 {
		c.logf("[artifact] SSE 沙箱产物: %v", sandboxNames(result.SandboxArtifacts))
		result.Text = ""
	}
	if result.ExpectGeneratedImages && len(result.ImageFileIDs) > 0 {
		c.logf("[artifact] 生图 file_id: %v", result.ImageFileIDs)
	}

	return result, nil
}

// getWsURL 调用 celsius/ws/user 获取 WebSocket 连接地址
func (c *Client) getWsURL() (string, error) {
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "*/*",
			"x-openai-target-path":  "/backend-api/celsius/ws/user",
			"x-openai-target-route": "/backend-api/celsius/ws/user",
		}).
		Get("/backend-api/celsius/ws/user")
	if err != nil {
		return "", fmt.Errorf("celsius/ws/user request: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("celsius/ws/user %d: %s", resp.StatusCode, truncateStr(resp.String(), 200))
	}
	var result struct {
		WebsocketURL string `json:"websocket_url"`
	}
	if err := json.Unmarshal(resp.Bytes(), &result); err != nil {
		return "", fmt.Errorf("parse celsius/ws/user: %w", err)
	}
	if result.WebsocketURL == "" {
		return "", fmt.Errorf("empty websocket_url")
	}
	return result.WebsocketURL, nil
}

// dialChatWS 获取 ws url 并完成握手+初始化订阅，返回已就绪的连接
func (c *Client) dialChatWS() (*websocket.Conn, error) {
	wsURL, err := c.getWsURL()
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
		NetDialContext:   c.dialContext,
	}
	hdrs := http.Header{}
	hdrs.Set("User-Agent", c.userAgent)
	hdrs.Set("Origin", "https://chatgpt.com")

	conn, _, err := dialer.Dial(wsURL, hdrs)
	if err != nil {
		return nil, fmt.Errorf("ws dial: %w", err)
	}

	// 初始化：connect + 订阅三个基础 topic
	initMsg := []map[string]interface{}{
		{"id": 1, "command": map[string]interface{}{
			"type":     "connect",
			"presence": map[string]string{"type": "presence", "state": "background"},
		}},
		{"id": 2, "command": map[string]interface{}{"type": "subscribe", "topic_id": "calpico-chatgpt"}},
		{"id": 3, "command": map[string]interface{}{"type": "subscribe", "topic_id": "conversations"}},
		{"id": 4, "command": map[string]interface{}{"type": "subscribe", "topic_id": "app_notifications"}},
	}
	if err := conn.WriteJSON(initMsg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ws init send: %w", err)
	}

	// 不等待初始化 reply，由 subscribeWSStream 的读取循环统一处理所有帧
	return conn, nil
}

// wsIDCounter 用于 WebSocket 命令 id 自增（跨调用）
var wsIDCounter int64 = 4

func nextWsID() int64 {
	return atomic.AddInt64(&wsIDCounter, 1)
}

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
		return nil, fmt.Errorf("conversation %d: %s", resp.StatusCode, truncateStr(string(b), 500))
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

	c.MergeApplyAndEmitArtifacts(result, opts)
	// 仅当 final 通道正文已完整时才跳过 WS catchup（避免未出 JSON 就 handoff）
	result.bodyStreamFromSSE = result.assistantFinalText != ""

	if handoffTopicID == "" && result.TurnExchangeID != "" {
		handoffTopicID = "conversation-turn-" + result.TurnExchangeID
		c.logf("[handoff] turn topic 来自 TurnExchangeID: %s", handoffTopicID)
	}

	needImageHandoff := !c.DisableAutoImage && result.ExpectGeneratedImages && (opts.ForcePictureV2 || result.ImageTaskID != "")
	sseCanSkipWS := needImageHandoff && result.CanSkipImageWSAfterSSE()

	if needImageHandoff && wsConn != nil && !sseCanSkipWS {
		// 长 SSE 期间 WS 无人读，服务端常会关连接；生图 handoff 前必须重连
		_ = wsConn.Close()
		var err error
		wsConn, err = c.dialChatWS()
		if err != nil {
			return nil, fmt.Errorf("生图 handoff 重连 WS: %w", err)
		}
		c.logf("[handoff] 生图前已重连 WebSocket")
	}

	if sseCanSkipWS {
		c.logf("[handoff] SSE 阶段已收齐出图 slots=%d file_ids=%v，跳过 WS 长等待", len(result.imageSlots), result.ImageFileIDs)
		c.FinishImageGenWS(result, opts)
	} else if handoffTopicID != "" && wsConn != nil {
		c.logf("[handoff] 订阅 WebSocket topic: %s", handoffTopicID)
		var err error
		// picture_v2 / 生图轮次必须走 image WS：subscribeWSStream 不处理 conversation-update，会漏掉 async 出图
		waitImageGen := !c.DisableAutoImage && result.ExpectGeneratedImages && (opts.ForcePictureV2 || result.ImageTaskID != "")
		if waitImageGen {
			c.logf("[handoff] 生图 WS 长等待 ForcePictureV2=%v ImageTaskID=%q topic=%s", opts.ForcePictureV2, result.ImageTaskID, handoffTopicID)
			err = c.subscribeWSImageCombined(wsConn, handoffTopicID, result.ConversationID, result, opts, &lastText, handler)
		} else {
			err = c.subscribeWSStream(wsConn, handoffTopicID, result, opts, &lastText, handler)
		}
		if err != nil {
			return nil, fmt.Errorf("ws stream: %w", err)
		}
		c.MergeApplyAndEmitArtifacts(result, opts)
		if result.ExpectGeneratedImages {
			if result.HasDalleGeneratedOutput() {
				c.logf("[artifact] 生图 file_id: %v", result.ImageFileIDs)
			} else if len(result.ImageFileIDs) > 0 {
				c.logf("[image-ws] 无 DALL·E 产出（勿将用户参考图 file_id 当作生图结果）: %v", result.ImageFileIDs)
			} else if result.imageAsyncTaskActive {
				c.logf("[image-ws] async 生图任务已启动但 WS 未收到带 gen_id 的图片更新")
			} else if opts.ForcePictureV2 {
				c.logf("[image-ws] picture_v2 已开启但未收到 DALL·E 出图（请检查 WS conversation-update）")
			}
		}
	} else if wsConn != nil && needImageHandoff && result.ConversationID != "" {
		c.logf("[handoff] 仍无 turn topic（TurnExchangeID=%q），仅监听 conversation-update conv=%s", result.TurnExchangeID, result.ConversationID)
		if err := c.subscribeWSConvUpdate(wsConn, result.ConversationID, result, opts, handler); err != nil {
			// 无 turn topic 时 conv-update 常因 WS 已断而失败；若 async 仍在跑，给明确提示
			if !result.HasDalleGeneratedOutput() {
				return nil, fmt.Errorf("ws conv-update（无 turn topic，图片可能仍在服务端生成）: %w", err)
			}
		}
		c.MergeApplyAndEmitArtifacts(result, opts)
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

// probeImageRouteFromSSE 诊断：区分 DALL·E async（应有 gen_id）与 Codex image_gen 容器路径（通常无 gen_id）。
func (c *Client) probeImageRouteFromSSE(payload string, result *ChatResult, opts ChatOptions) {
	if result == nil || (!opts.ForcePictureV2 && !result.ExpectGeneratedImages) {
		return
	}
	lower := strings.ToLower(payload)
	switch {
	case strings.Contains(lower, "image_gen_task_id") || strings.Contains(lower, "ghostrider"):
		if !result.sawImageGenTool {
			c.logf("[image-route] dalle_async（预期 conversation-update + gen_id）model=%s ForcePictureV2=%v", c.model, opts.ForcePictureV2)
		}
		result.sawImageGenTool = true
	case strings.Contains(lower, `"name": "image_gen"`) || strings.Contains(lower, `"name":"image_gen"`):
		if !result.sawImageGenTool {
			c.logf("[image-route] codex_image_gen 工具（built-in image_gen，通常无 dalle.gen_id）model=%s", c.model)
		}
		result.sawImageGenTool = true
	case strings.Contains(lower, "container.exec") && strings.Contains(lower, "imagegen"):
		c.logf("[image-route] codex_container 读取 imagegen skill（thinking+picture_v2 常见路径，非 classic DALL·E）model=%s", c.model)
	case strings.Contains(lower, `"name": "dalle"`) || strings.Contains(lower, `"name":"dalle"`):
		c.logf("[image-route] dalle 工具调用 model=%s", c.model)
		result.sawImageGenTool = true
	}
}

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

func (c *Client) handleImageWSReadError(err error, result *ChatResult, opts ChatOptions, waitStart time.Time, tag string) (handled bool, retErr error) {
	if err == nil || result == nil {
		return false, nil
	}
	if result.HasDalleGeneratedOutput() {
		c.FinishImageGenWS(result, opts)
		c.logf("[image-ws] WS 断开但已有出图，正常结束 tag=%s err=%v", tag, err)
		return true, nil
	}
	if done, finishErr := c.tryFinishImageGenWS(result, opts, waitStart, tag+"_ws_closed"); done {
		c.logf("[image-ws] WS 关闭后 idle 收齐 tag=%s err=%v", tag, err)
		return true, finishErr
	}
	return false, nil
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

// parseWSFrames 将 WebSocket 文本帧解析为帧列表（支持 JSON 数组或单对象）
func parseWSFrames(raw []byte) []map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var frames []map[string]interface{}
		if err := json.Unmarshal(raw, &frames); err != nil {
			return nil
		}
		return frames
	}
	var single map[string]interface{}
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil
	}
	return []map[string]interface{}{single}
}

func (c *Client) bumpImageGenActivity(result *ChatResult) {
	if result == nil {
		return
	}
	result.lastImageGenActivityAt = time.Now().UnixNano()
}

func isImageAsyncWSUpdate(updateType string) bool {
	if strings.HasPrefix(updateType, "async-task-") {
		return true
	}
	switch updateType {
	case "add-messages", "insert-message", "update-message":
		return true
	default:
		return false
	}
}

func (c *Client) trackImageAsyncTaskUpdate(result *ChatResult, updateType string) {
	if result == nil || updateType == "" || !isImageAsyncWSUpdate(updateType) {
		return
	}
	// 不在此处 bump：add-messages 刷屏会导致 idle 永不满足；仅在新 file_id 修订时 bump
	switch updateType {
	case "async-task-start":
		result.imageAsyncTaskPending++
		result.imageAsyncTaskActive = true
		c.logf("[image-ws][async] start pending=%d", result.imageAsyncTaskPending)
	case "async-task-complete", "async-task-end", "async-task-finished", "async-task-stop", "async-task-done", "async-task-success":
		result.imageGenAsyncCompleteSeen = true
		if result.imageAsyncTaskPending > 0 {
			result.imageAsyncTaskPending--
		}
		if result.imageAsyncTaskPending <= 0 {
			result.imageAsyncTaskPending = 0
			result.imageAsyncTaskActive = false
		}
		c.logf("[image-ws][async] complete type=%s pending=%d active=%v", updateType, result.imageAsyncTaskPending, result.imageAsyncTaskActive)
	default:
		// add-messages / update-message：仅刷新活动时间，不增加 pending（避免无 complete 时永久卡住）
		c.logf("[image-ws][async] progress type=%s pending=%d active=%v", updateType, result.imageAsyncTaskPending, result.imageAsyncTaskActive)
	}
}

// handleSetConversationAsyncStatus 网页端生图结束时常见：conversation_async_status=4（见 testdata ws.ndjson）。
func (c *Client) handleSetConversationAsyncStatus(payload map[string]interface{}, result *ChatResult) {
	uc, _ := payload["update_content"].(map[string]interface{})
	status := -1
	switch v := uc["conversation_async_status"].(type) {
	case float64:
		status = int(v)
	case int:
		status = v
	}
	c.logf("[image-ws][async] set-conversation-async-status status=%d", status)
	// 抓包中完成态为 4；其它值仅记录，避免误判
	if status == 4 {
		result.imageGenConvAsyncStatusDone = true
		result.imageGenAsyncCompleteSeen = true
		result.imageAsyncTaskPending = 0
		result.imageAsyncTaskActive = false
		result.imageGenConvStatusAt = time.Now().UnixNano()
		c.logf("[image-ws][async] 生图任务完成（async_status=4），将在本批 WS 处理结束后返回客户端")
	}
}

// processConvUpdatePayload 处理 conversation-update 的 payload（生图可多图，不在此结束 WS）。
func (c *Client) processConvUpdatePayload(payload map[string]interface{}, result *ChatResult, opts ChatOptions, handler StreamHandler) {
	result.ExpectGeneratedImages = IsGeneratedImageTurn(result.ArtifactSignals, opts)
	updateType, _ := payload["update_type"].(string)
	if updateType == "set-conversation-async-status" {
		c.handleSetConversationAsyncStatus(payload, result)
	}
	c.trackImageAsyncTaskUpdate(result, updateType)
	summary := summarizeConvUpdatePayload(payload)
	if summary != "" {
		c.logf("[image-ws][evt] %s pending=%d slots=%d", summary, result.imageAsyncTaskPending, len(result.imageSlots))
	}
	if updateType != "" && !isImageAsyncWSUpdate(updateType) {
		lower := strings.ToLower(updateType)
		if strings.Contains(lower, "complete") || strings.Contains(lower, "end") || strings.Contains(lower, "finish") {
			c.logf("[image-ws][evt] 未识别的结束类事件 type=%s（请反馈完整 update_type）", updateType)
		}
	}
	updateContent, ok := payload["update_content"].(map[string]interface{})
	if !ok {
		return
	}
	if msg, ok := updateContent["message"].(map[string]interface{}); ok {
		c.processConvUpdateMessage(msg, result, opts, handler, updateType)
		return
	}
	messages, ok := updateContent["messages"].([]interface{})
	if !ok {
		return
	}
	for _, msgI := range messages {
		msg, ok := msgI.(map[string]interface{})
		if !ok {
			continue
		}
		c.processConvUpdateMessage(msg, result, opts, handler, updateType)
	}
}

// tryFinishImageGenWS 若已满足结束条件则收尾并退出 WS 循环。
func (c *Client) tryFinishImageGenWS(result *ChatResult, opts ChatOptions, waitStart time.Time, tag string) (bool, error) {
	if result == nil || !result.CanImageGenIdleExit() {
		return false, nil
	}
	c.FinishImageGenWS(result, opts)
	c.logf("[image-ws] 生图收齐 %d 槽（%s 已等待 %ds pending=%d convStatus=%v）",
		len(result.imageSlots), tag, int(time.Since(waitStart).Seconds()),
		result.imageAsyncTaskPending, result.imageGenConvAsyncStatusDone)
	c.logImageGenDiag(result, "exit_ok_"+tag)
	return true, nil
}

// FinishImageGenWS 生图 WS 结束或 HTTP 收尾：定稿各槽位并刷新 ImageFileIDs。
func (c *Client) FinishImageGenWS(result *ChatResult, opts ChatOptions) {
	if result == nil || !result.ExpectGeneratedImages {
		return
	}
	c.FinalizeImageGenSlots(result, opts)
	result.RebuildImageFileIDsFromSlots()
}

func (c *Client) processConvUpdateMessage(msg map[string]interface{}, result *ChatResult, opts ChatOptions, handler StreamHandler, wsUpdateType string) {
	msgID, _ := msg["id"].(string)
	if result.ExpectGeneratedImages {
		c.tryNoteGeneratedImagesFromMessage(msg, result, opts, wsUpdateType)
		if meta, ok := msg["metadata"].(map[string]interface{}); ok {
			if refs, ok := meta["content_references"].([]interface{}); ok {
				for _, refRaw := range refs {
					ref, _ := refRaw.(map[string]interface{})
					if ap, _ := ref["asset_pointer"].(string); ap != "" {
						if fileID := extractFileID(ap); fileID != "" {
							c.logf("[image-ws] content_reference asset: %s", fileID)
							c.noteGeneratedImageRevision(result, opts, ParsedGeneratedImage{
								FileID: fileID, MessageID: msgID,
							}, wsUpdateType)
						}
					}
				}
			}
		}
	}
	author, _ := msg["author"].(map[string]interface{})
	role, _ := author["role"].(string)
	channel, _ := msg["channel"].(string)
	msgContent, _ := msg["content"].(map[string]interface{})
	parts, _ := msgContent["parts"].([]interface{})

	if channel == "analysis" {
		for _, part := range parts {
			if text, ok := part.(string); ok && text != "" {
				if handler != nil {
					handler(text)
				}
			}
		}
		return
	}

	if role == "tool" {
		name, _ := author["name"].(string)
		lowerName := strings.ToLower(name)
		if strings.Contains(lowerName, "dalle") || strings.Contains(lowerName, "image_gen") {
			result.sawImageGenTool = true
		}
		status, _ := msg["status"].(string)
		isImageTool := strings.Contains(lowerName, "dalle") || strings.Contains(lowerName, "image_gen")
		if isImageTool && status == "in_progress" && !result.DalleStarted {
			title := "正在生成图片，请稍候..."
			for _, p := range parts {
				if pStr, ok := p.(string); ok && pStr != "" {
					title = "正在生成图片: " + pStr
					break
				}
			}
			opts.Artifacts.normalized().emit(StreamEvent{
				Event: StreamEventArtifactPending,
				Kind:  "generated_image",
				Title: title,
			})
			if handler != nil {
				handler("\n\n[" + title + "...]\n\n")
			}
			result.DalleStarted = true
		}
	}
}

// subscribeWSImageCombined 生图：订阅 conversation-turn-* 消费流式 delta，同时处理 conversation-update 拿图片
func (c *Client) subscribeWSImageCombined(conn *websocket.Conn, turnTopicID, conversationID string, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler) error {
	subID := nextWsID()
	subMsg := []map[string]interface{}{
		{"id": subID, "command": map[string]interface{}{
			"type":     "subscribe",
			"topic_id": turnTopicID,
			"offset":   "0",
		}},
	}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("ws subscribe send: %w", err)
	}

	var useDeltaEncoding bool
	var currentEvent string

	const totalTimeout = 10 * time.Minute
	const pingInterval = 25 * time.Second
	const readDeadlineExt = 60 * time.Second
	deadline := time.Now().Add(totalTimeout)

	imageGenReadWait := func() time.Duration {
		if result == nil {
			return readDeadlineExt
		}
		if result.imageGenConvAsyncStatusDone || result.imageGenAsyncCompleteSeen {
			return 5 * time.Second
		}
		if result.imageGenTurnDone && result.HasDalleGeneratedOutput() && result.lastImageGenActivityAt > 0 {
			since := time.Since(time.Unix(0, result.lastImageGenActivityAt))
			need := 3 * time.Second
			if since >= need {
				return 200 * time.Millisecond
			}
			return need - since
		}
		return readDeadlineExt
	}

	conn.SetPongHandler(func(string) error {
		// 已有图且 turn 结束：勿用 pong 无限续期 ReadMessage，否则无法 idle 退出
		if result != nil && result.imageGenTurnDone && result.HasDalleGeneratedOutput() {
			return nil
		}
		conn.SetReadDeadline(time.Now().Add(readDeadlineExt))
		return nil
	})
	stopPing := make(chan struct{})
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			case <-stopPing:
				return
			}
		}
	}()
	defer close(stopPing)

	conn.SetReadDeadline(time.Now().Add(readDeadlineExt))
	defer conn.SetReadDeadline(time.Time{})

	waitStart := time.Now()
	lastProgress := time.Now()
	lastDiag := time.Now()
	var turnDoneAt time.Time
	c.logImageGenDiag(result, "ws_loop_start")
	for {
		if time.Now().After(deadline) {
			c.logImageGenDiag(result, "timeout")
			return fmt.Errorf("超过最大等待时间 %.0f 分钟，图片未返回", totalTimeout.Minutes())
		}
		if cleared := result.MaybeClearStaleImageAsyncPending(); cleared {
			c.logf("[image-ws][async] 长期无 complete，已清除 stale pending（有图且 idle≥20s）")
			c.logImageGenDiag(result, "stale_pending_cleared")
		}
		if result.imageGenTurnDone && turnDoneAt.IsZero() {
			turnDoneAt = time.Now()
		}
		if !result.imageGenConvFetched && result.ConversationID != "" && !result.HasDalleGeneratedOutput() {
			shouldFetch := false
			if result.imageGenTurnDone && !turnDoneAt.IsZero() && time.Since(turnDoneAt) >= 5*time.Second {
				shouldFetch = true
			} else if time.Since(waitStart) >= 90*time.Second {
				shouldFetch = true
			}
			if shouldFetch {
				result.imageGenConvFetched = true
				c.tryFetchGeneratedImagesFromConversation(result, opts)
				c.MergeApplyAndEmitArtifacts(result, opts)
			}
		}
		if done, err := c.tryFinishImageGenWS(result, opts, waitStart, "loop_top"); done {
			return err
		}
		readWait := imageGenReadWait()
		if readWait < 200*time.Millisecond {
			readWait = 200 * time.Millisecond
		}
		conn.SetReadDeadline(time.Now().Add(readWait))
		if time.Since(lastProgress) >= 15*time.Second {
			c.logf("[image-ws] 等待生图中... 已等待 %ds | %s",
				int(time.Since(waitStart).Seconds()), result.ImageGenExitBlockReason())
			lastProgress = time.Now()
		}
		if time.Since(lastDiag) >= 30*time.Second {
			c.logImageGenDiag(result, "heartbeat")
			lastDiag = time.Now()
		}
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if handled, herr := c.handleImageWSReadError(err, result, opts, waitStart, "image_combined"); handled {
				return herr
			}
			return fmt.Errorf("ws read: %w", err)
		}
		// read deadline 已在 loop_top 按 idle 退出策略设置

		frames := parseWSFrames(raw)
		c.logAndRecordWSFrames(raw, frames)
		for _, frame := range frames {
			fType, _ := frame["type"].(string)
			switch fType {
			case "conversation-update":
				payload, ok := frame["payload"].(map[string]interface{})
				if !ok {
					continue
				}
				if cid, _ := payload["conversation_id"].(string); cid != conversationID {
					continue
				}
				c.processConvUpdatePayload(payload, result, opts, handler)
			case "reply":
				reply, ok := frame["reply"].(map[string]interface{})
				if !ok {
					continue
				}
				replyTopicID, _ := reply["topic_id"].(string)
				if replyTopicID != turnTopicID {
					continue
				}
				catchups, _ := reply["catchups"].([]interface{})
				if result.bodyStreamFromSSE {
					c.logf("[ws] skip catchups=%d (final body already from HTTP SSE)", len(catchups))
				} else {
					c.logf("[ws] reply catchups=%d", len(catchups))
					for _, cu := range catchups {
						if msg, ok := cu.(map[string]interface{}); ok {
							if c.processWSMessage(msg, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent) {
								result.imageGenTurnDone = true
							}
						}
					}
				}
			case "message":
				frameTopic, _ := frame["topic_id"].(string)
				if frameTopic != turnTopicID {
					continue
				}
				if c.processWSMessage(frame, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent) {
					result.imageGenTurnDone = true
					c.logf("[image-ws] turn topic 流已 [DONE]")
				}
			}
		}
		c.MergeApplyAndEmitArtifacts(result, opts)
		if done, err := c.tryFinishImageGenWS(result, opts, waitStart, "after_frames"); done {
			return err
		}
	}
}

func imageFileIDSeen(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// logAndRecordWSFrames 打印并可选落盘 WebSocket 帧（stream-capture 写 ws.ndjson）。
func (c *Client) logAndRecordWSFrames(raw []byte, frames []map[string]interface{}) {
	rawStr := string(raw)
	hasImg := strings.Contains(rawStr, "sediment://") || strings.Contains(rawStr, "image_asset_pointer")
	if len(frames) == 0 {
		c.logf("[ws-frame] (unparsed) raw_len=%d has_image_ref=%v", len(raw), hasImg)
		if c.StreamRecorder != nil {
			c.StreamRecorder.RecordWS("", raw)
		}
		return
	}
	for _, frame := range frames {
		fType, _ := frame["type"].(string)
		c.logf("[ws-frame] type=%s raw_len=%d has_image_ref=%v", fType, len(raw), hasImg)
		if c.StreamRecorder != nil {
			c.StreamRecorder.RecordWS(fType, raw)
		}
	}
}

// subscribeWSStream 通过已有 WebSocket 连接订阅 topic 并消费 encoded_item 里的 SSE 数据
func (c *Client) subscribeWSStream(conn *websocket.Conn, topicID string, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler) error {
	subID := nextWsID()
	subMsg := []map[string]interface{}{
		{"id": subID, "command": map[string]interface{}{
			"type":     "subscribe",
			"topic_id": topicID,
			"offset":   "0",
		}},
	}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("ws subscribe send: %w", err)
	}

	var useDeltaEncoding bool
	var currentEvent string
	done := false

	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	for !done {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("ws read: %w", err)
		}

		conn.SetReadDeadline(time.Now().Add(120 * time.Second))

		frames := parseWSFrames(raw)
		if len(frames) == 0 {
			continue
		}

		for _, frame := range frames {
			fType, _ := frame["type"].(string)

			if fType == "reply" {
				reply, ok := frame["reply"].(map[string]interface{})
				if !ok {
					continue
				}
				replyTopicID, _ := reply["topic_id"].(string)
				if replyTopicID != topicID {
					continue
				}
				catchups, _ := reply["catchups"].([]interface{})
				if result.bodyStreamFromSSE && !result.ExpectGeneratedImages {
					c.logf("[ws] skip catchups=%d (final body already from HTTP SSE)", len(catchups))
					done = true
				} else {
					c.logf("[ws] reply catchups=%d", len(catchups))
					for _, cu := range catchups {
						if msg, ok := cu.(map[string]interface{}); ok {
							d := c.processWSMessage(msg, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent)
							if d {
								done = true
							}
						}
					}
					// catchup 含完整流但无 [DONE] 标记时，正文已到齐即可结束
					if !done && result.assistantFinalText != "" {
						c.logf("[ws] catchups done, final len=%d", len([]rune(result.assistantFinalText)))
						done = true
					}
				}
				continue
			}

			if fType == "message" {
				frameTopic, _ := frame["topic_id"].(string)
				if frameTopic != topicID {
					continue
				}
				d := c.processWSMessage(frame, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent)
				if d {
					done = true
				} else if result.assistantFinalText != "" {
					done = true
				}
			}
		}
	}

	return nil
}

// subscribeWSConvUpdate 监听 WebSocket 的 conversation-update 消息（生图场景，无 turn topic 时）
// 通过定期 Ping 心跳保活连接，最长等待 10 分钟。
func (c *Client) subscribeWSConvUpdate(conn *websocket.Conn, conversationID string, result *ChatResult, opts ChatOptions, handler StreamHandler) error {
	const totalTimeout = 10 * time.Minute
	const pingInterval = 25 * time.Second
	const readDeadlineExt = 60 * time.Second

	deadline := time.Now().Add(totalTimeout)

	// Pong handler：收到服务端 pong 后重置读 deadline
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(readDeadlineExt))
		return nil
	})

	// 心跳 goroutine：每 25s 发一次 Ping
	stopPing := make(chan struct{})
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
					return
				}
			case <-stopPing:
				return
			}
		}
	}()
	defer close(stopPing)

	conn.SetReadDeadline(time.Now().Add(readDeadlineExt))
	defer conn.SetReadDeadline(time.Time{})

	waitStart := time.Now()
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("超过最大等待时间 %.0f 分钟，图片未返回", totalTimeout.Minutes())
		}
		if done, err := c.tryFinishImageGenWS(result, opts, waitStart, "conv_loop_top"); done {
			return err
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			if handled, herr := c.handleImageWSReadError(err, result, opts, waitStart, "conv_update"); handled {
				return herr
			}
			return fmt.Errorf("ws read: %w", err)
		}
		readWait := readDeadlineExt
		if result.imageGenConvAsyncStatusDone || result.imageGenAsyncCompleteSeen {
			readWait = 5 * time.Second
		}
		conn.SetReadDeadline(time.Now().Add(readWait))

		for _, frame := range parseWSFrames(raw) {
			if fType, _ := frame["type"].(string); fType != "conversation-update" {
				continue
			}
			payload, ok := frame["payload"].(map[string]interface{})
			if !ok {
				continue
			}
			if cid, _ := payload["conversation_id"].(string); cid != conversationID {
				continue
			}
			c.processConvUpdatePayload(payload, result, opts, handler)
		}
		c.MergeApplyAndEmitArtifacts(result, opts)
		if done, err := c.tryFinishImageGenWS(result, opts, waitStart, "conv_after_frames"); done {
			return err
		}
	}
}

// processWSMessage 处理单条 WebSocket message 帧，返回 true 表示流结束
func (c *Client) processWSMessage(frame map[string]interface{}, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler, useDeltaEncoding *bool, currentEvent *string) bool {
	payload1, ok := frame["payload"].(map[string]interface{})
	if !ok {
		c.probeUnhandledWSImageFrame(frame, result, "no_payload")
		return false
	}
	if payload2, ok := payload1["payload"].(map[string]interface{}); ok {
		if encoded, ok := payload2["encoded_item"].(string); ok && encoded != "" {
			return c.processWSEncodedItem(encoded, result, opts, lastText, handler, useDeltaEncoding, currentEvent)
		}
		if msg, ok := payload2["message"].(map[string]interface{}); ok {
			c.ingestWSMessageObject(msg, result, opts, handler, lastText, "ws_payload_message")
			return false
		}
	}
	if msg, ok := payload1["message"].(map[string]interface{}); ok {
		c.ingestWSMessageObject(msg, result, opts, handler, lastText, "ws_direct_message")
		return false
	}
	c.probeUnhandledWSImageFrame(frame, result, "no_encoded_item")
	return false
}

func (c *Client) ingestWSMessageObject(msg map[string]interface{}, result *ChatResult, opts ChatOptions, handler StreamHandler, lastText *string, via string) {
	if msg == nil || result == nil {
		return
	}
	result.noteTurnExchangeFromMessage(msg)
	if result.ExpectGeneratedImages {
		c.tryNoteGeneratedImagesFromMessage(msg, result, opts, via)
	}
	c.processConvUpdateMessage(msg, result, opts, handler, via)
	if author := getNestedString(msg, "author", "role"); author == "assistant" {
		if text := getFirstStringPart(msg); text != "" {
			channel, _ := msg["channel"].(string)
			if channel == "final" {
				c.emitBodyFull(result, lastText, text, "final", handler)
			}
		}
	}
}

func (c *Client) probeUnhandledWSImageFrame(frame map[string]interface{}, result *ChatResult, reason string) {
	if c == nil || frame == nil || result == nil || !result.ExpectGeneratedImages {
		return
	}
	raw, _ := json.Marshal(frame)
	s := string(raw)
	if !strings.Contains(s, "image_asset_pointer") && !strings.Contains(s, "sediment://") {
		return
	}
	fType, _ := frame["type"].(string)
	topic, _ := frame["topic_id"].(string)
	keys := []string{}
	if p1, ok := frame["payload"].(map[string]interface{}); ok {
		for k := range p1 {
			keys = append(keys, "p1."+k)
		}
		if p2, ok := p1["payload"].(map[string]interface{}); ok {
			for k := range p2 {
				keys = append(keys, "p2."+k)
			}
		}
	}
	c.logf("[image-ws][probe] 帧含图但未解析 reason=%s type=%s topic=%s keys=%v slots=%d",
		reason, fType, topic, keys, len(result.imageSlots))
}

func (c *Client) processWSEncodedItem(encoded string, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler, useDeltaEncoding *bool, currentEvent *string) bool {
	for _, line := range strings.Split(encoded, "\n") {
		line = strings.TrimRight(line, "\r")

		if strings.HasPrefix(line, "event: ") {
			*currentEvent = strings.TrimSpace(line[7:])
			if *currentEvent == "delta_encoding" {
				*useDeltaEncoding = true
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		ssePayload := strings.TrimSpace(line[6:])
		if ssePayload == "" || ssePayload == `"v1"` {
			continue
		}
		if ssePayload == "[DONE]" {
			return true
		}

		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(ssePayload), &evt); err != nil {
			*currentEvent = ""
			continue
		}
		result.ArtifactSignals = MergeSignals(result.ArtifactSignals, ExtractSignalsFromJSON(evt))
		c.MergeApplyAndEmitArtifacts(result, opts)

		c.noteConversationID(result, opts, evt)

		evtType, _ := evt["type"].(string)
		if evtType == "resume_conversation_token" || evtType == "stream_handoff" {
			*currentEvent = ""
			continue
		}

		checkImageTaskID(evt, result)
		if *useDeltaEncoding && *currentEvent == "delta" {
			c.processDeltaSSE(evt, result, opts, lastText, handler)
		} else {
			c.processFullSSE(evt, result, opts, lastText, handler)
		}
		*currentEvent = ""
	}
	return false
}

// parseStreamHandoff 从 stream_handoff 事件中提取 resume_sse_endpoint 的 topic_id
func parseStreamHandoff(evt map[string]interface{}) (bool, string) {
	options, ok := evt["options"].([]interface{})
	if !ok {
		return false, ""
	}
	for _, optRaw := range options {
		opt, ok := optRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if typ, _ := opt["type"].(string); typ == "subscribe_ws_topic" {
			topicID, _ := opt["topic_id"].(string)
			return topicID != "", topicID
		}
	}
	return false, ""
}

// checkImageTaskID 从 SSE 事件中提取图片任务 ID（兼容旧版 image_gen_task_id 和新版 ghostrider）
func checkImageTaskID(evt map[string]interface{}, result *ChatResult) {
	extractFromMeta := func(meta map[string]interface{}) {
		if tid, ok := meta["image_gen_task_id"].(string); ok && tid != "" {
			result.ImageTaskID = tid
			return
		}
		if result.ImageTaskID == "" {
			if _, ok := meta["ghostrider"]; ok {
				result.ImageTaskID = "ghostrider"
			}
		}
	}

	if v, ok := evt["v"].(map[string]interface{}); ok {
		if msg, ok := v["message"].(map[string]interface{}); ok {
			if meta, ok := msg["metadata"].(map[string]interface{}); ok {
				extractFromMeta(meta)
			}
		}
	}
}

func (result *ChatResult) noteAssistantChannel(channel string) {
	if channel == "analysis" {
		result.sawAnalysisChannel = true
	}
	if channel != "" {
		result.deltaChannel = channel
	}
}

func (result *ChatResult) isAnalysisStream() bool {
	if result.deltaChannel == "analysis" {
		return true
	}
	return result.deltaChannel == "" && result.sawAnalysisChannel
}

func (c *Client) emitThinkingDelta(result *ChatResult, text string, handler StreamHandler) {
	if text == "" {
		return
	}
	result.ThinkingText += text
	if handler != nil {
		handler("\x00THINK\x00" + text)
	}
}

func (result *ChatResult) shouldSkipImageGenToolBodyDelta(text string) bool {
	if !result.ExpectGeneratedImages {
		return false
	}
	if result.deltaChannel == "commentary" {
		return true
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// ImageGen 工具参数 JSON 碎片（勿下发给客户端）
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, ",") || strings.Contains(t, "referenced_image_ids") {
		return true
	}
	return false
}

func (c *Client) emitBodyDelta(result *ChatResult, lastText *string, text string, handler StreamHandler) {
	if text == "" || result.shouldSkipImageGenToolBodyDelta(text) {
		return
	}
	newText := *lastText + text
	if newText == *lastText {
		return
	}
	toEmit := newText[result.emittedBodyLen:]
	*lastText = newText
	if result.deltaChannel == "final" {
		result.assistantFinalText = newText
	}
	if len(toEmit) == 0 {
		return
	}
	result.emittedBodyLen = len(newText)
	if handler != nil {
		handler(toEmit)
	}
}

func (c *Client) emitBodyFull(result *ChatResult, lastText *string, text, channel string, handler StreamHandler) {
	if text == "" {
		return
	}
	if text == *lastText {
		return
	}
	if len(text) < len(*lastText) && strings.HasPrefix(*lastText, text) {
		return
	}
	if len(text) <= len(*lastText) {
		return
	}
	*lastText = text
	if channel == "final" {
		result.assistantFinalText = text
		c.logf("[reply-final] channel=final len=%d", len([]rune(text)))
	}
	toEmit := text[result.emittedBodyLen:]
	if len(toEmit) == 0 {
		return
	}
	result.emittedBodyLen = len(text)
	if handler != nil {
		handler(toEmit)
	}
}

// processDeltaSSE 处理 delta 编码模式的 SSE 事件
// ChatGPT delta 格式有多种变体：
//  A) 顶层 patch：{"p":"/message/content/parts/0","o":"append","v":"text"}
//  B) 简写 append：{"v":"text"}（省略 p/o，隐含对 parts/0 的追加）
//  C) 消息对象 add：{"p":"","o":"add","v":{"message":{...}}}
//  D) 完成 patch 数组：{"p":"","o":"patch","v":[...patches...]}
func (c *Client) processDeltaSSE(evt map[string]interface{}, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler) {
	pPath, _ := evt["p"].(string)
	pOp, _ := evt["o"].(string)

	// 格式 A：顶层 append patch
	if pOp == "append" {
		if result.ExpectGeneratedImages && (pPath == "/message/content/text" || strings.HasPrefix(pPath, "/message/content/text")) {
			return
		}
		if pPath == "/message/content/parts/0" {
			if text, ok := evt["v"].(string); ok && text != "" {
				if result.isAnalysisStream() {
					c.emitThinkingDelta(result, text, handler)
				} else {
					c.emitBodyDelta(result, lastText, text, handler)
				}
			}
			return
		}
	}

	v := evt["v"]

	// 格式 B：只有 v 字段，且是字符串 → 隐含 append
	_, hasP := evt["p"]
	_, hasO := evt["o"]
	if !hasP && !hasO {
		if text, ok := v.(string); ok && text != "" {
			if result.isAnalysisStream() {
				c.emitThinkingDelta(result, text, handler)
			} else {
				c.emitBodyDelta(result, lastText, text, handler)
			}
			return
		}
	}

	// 格式 C：v 是包含 message 的 map（消息对象初始化或 final channel）
	if vMap, ok := v.(map[string]interface{}); ok {
		if msgRaw, exists := vMap["message"]; exists {
			if msg, ok := msgRaw.(map[string]interface{}); ok {
				result.noteTurnExchangeFromMessage(msg)
				author := getNestedString(msg, "author", "role")
				channel, _ := msg["channel"].(string)
				msgID, _ := msg["id"].(string)

				if author == "assistant" && msgID != "" {
					result.LastAssistantMsgID = msgID
					result.noteAssistantChannel(channel)

					// content_type="thoughts"：解析思考步骤（summary + content）
					if content, ok := msg["content"].(map[string]interface{}); ok {
						if ct, _ := content["content_type"].(string); ct == "thoughts" {
							if thoughts, ok := content["thoughts"].([]interface{}); ok {
								c.extractThoughts(thoughts, result, handler)
							}
						}
					}
					if result.ExpectGeneratedImages {
						c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "ws_turn_delta")
					}
				}
				if author == "tool" {
					if name := getNestedString(msg, "author", "name"); name != "" {
						lower := strings.ToLower(name)
						if strings.Contains(lower, "dalle") || strings.Contains(lower, "image_gen") {
							result.sawImageGenTool = true
						}
					}
					if result.ExpectGeneratedImages {
						c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "sse_tool_add")
					}
					if meta, ok := msg["metadata"].(map[string]interface{}); ok {
						if tid, ok := meta["image_gen_task_id"].(string); ok && tid != "" {
							result.ImageTaskID = tid
						}
						// 新版 ghostrider 异步生图：没有 image_gen_task_id，用 "ghostrider" 作为触发标志
						if result.ImageTaskID == "" {
							if _, ok := meta["ghostrider"]; ok {
								result.ImageTaskID = "ghostrider"
							}
						}
						// 思考模型：reasoning_title 是每步工具调用的思考标题
						if title, ok := meta["reasoning_title"].(string); ok && title != "" {
							// 同时取 content.parts[0] 作为执行输出
							execOutput := ""
							if content, ok := msg["content"].(map[string]interface{}); ok {
								if text, ok := content["text"].(string); ok {
									execOutput = text
								} else if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
									if s, ok := parts[0].(string); ok {
										execOutput = s
									}
								}
							}
							if handler != nil {
								payload := title
								if execOutput != "" {
									payload += "\x1F" + execOutput // \x1F 单元分隔符
								}
								handler("\x00THINK_STEP\x00" + payload)
							}
						}
					}
				}
				// final channel 上的完整文本（网页端可见的最终 JSON/正文）
				if author == "assistant" && channel == "final" {
					if text := getFirstStringPart(msg); text != "" {
						result.noteAssistantChannel("final")
						c.emitBodyFull(result, lastText, text, "final", handler)
					}
				}
			}
		}
	}

	// 格式 D：v 是 patches 数组（批量 patch）
	if patches, ok := v.([]interface{}); ok {
		for _, p := range patches {
			if patch, ok := p.(map[string]interface{}); ok {
				pp, _ := patch["p"].(string)
				po, _ := patch["o"].(string)
				if result.ExpectGeneratedImages && po == "append" && (pp == "/message/content/text" || strings.HasPrefix(pp, "/message/content/text")) {
					continue
				}
				if pp == "/message/content/parts/0" && po == "append" {
					if text, ok := patch["v"].(string); ok && text != "" {
						if result.isAnalysisStream() {
							c.emitThinkingDelta(result, text, handler)
						} else {
							c.emitBodyDelta(result, lastText, text, handler)
						}
					}
					continue
				}
				if result.ExpectGeneratedImages && strings.HasPrefix(pp, "/message/content/parts/") &&
					(po == "append" || po == "add" || po == "replace") {
					if partMap, ok := patch["v"].(map[string]interface{}); ok {
						if partMap["content_type"] == "image_asset_pointer" {
							ap, _ := partMap["asset_pointer"].(string)
							if fileID := extractFileID(ap); fileID != "" {
								p := ParsedGeneratedImage{FileID: fileID}
								if meta, ok := partMap["metadata"].(map[string]interface{}); ok {
									if dalle, ok := meta["dalle"].(map[string]interface{}); ok {
										p.GenID, _ = dalle["gen_id"].(string)
									}
								}
								c.noteGeneratedImageRevision(result, opts, p, "ws_part_patch")
							}
							continue
						}
					}
				}
			}
		}
	}
}

// processFullSSE 处理非 delta 编码模式的 SSE 事件
func (c *Client) processFullSSE(evt map[string]interface{}, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler) {
	msgRaw, exists := evt["message"]
	if !exists {
		return
	}
	msg, ok := msgRaw.(map[string]interface{})
	if !ok {
		return
	}

	author := getNestedString(msg, "author", "role")
	channel, _ := msg["channel"].(string)
	msgID, _ := msg["id"].(string)

	if author == "assistant" && msgID != "" {
		result.LastAssistantMsgID = msgID

		// content_type="thoughts"：解析思考步骤（summary + content）
		if content, ok := msg["content"].(map[string]interface{}); ok {
			if ct, _ := content["content_type"].(string); ct == "thoughts" {
				if thoughts, ok := content["thoughts"].([]interface{}); ok {
					c.extractThoughts(thoughts, result, handler)
				}
			}
		}
		if result.ExpectGeneratedImages {
			c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "ws_turn_full")
		}
	}

	if meta, ok := msg["metadata"].(map[string]interface{}); ok {
		if tid, ok := meta["image_gen_task_id"].(string); ok && tid != "" {
			result.ImageTaskID = tid
		}
		// 思考模型：tool 消息中的 reasoning_title 是每步思考标题
		if author == "tool" {
			if result.ExpectGeneratedImages {
				c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "sse_tool_full")
			}
			if title, ok := meta["reasoning_title"].(string); ok && title != "" {
				execOutput := ""
				if content, ok := msg["content"].(map[string]interface{}); ok {
					if text, ok := content["text"].(string); ok {
						execOutput = text
					} else if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
						if s, ok := parts[0].(string); ok {
							execOutput = s
						}
					}
				}
				if handler != nil {
					payload := title
					if execOutput != "" {
						payload += "\x1F" + execOutput
					}
					handler("\x00THINK_STEP\x00" + payload)
				}
			}
		}
	}

	if author == "assistant" {
		text := getFirstStringPart(msg)
		if text == "" {
			return
		}
		result.noteAssistantChannel(channel)
		if channel == "analysis" {
			if len(text) > len(result.ThinkingText) {
				c.emitThinkingDelta(result, text[len(result.ThinkingText):], handler)
				result.ThinkingText = text
			}
		} else if channel == "final" {
			c.emitBodyFull(result, lastText, text, "final", handler)
		} else if !result.sawAnalysisChannel {
			c.emitBodyFull(result, lastText, text, channel, handler)
		}
	}
}


// fetchTextdocs 调用 textdocs API 获取思考步骤的详细内容
// textdocs 返回一个对象数组，每个对象包含 type、thought（含 summary/content）等字段
func (c *Client) fetchTextdocs(conversationID string) ([]ThinkStep, error) {
	apiPath := "/backend-api/conversation/" + conversationID + "/textdocs"
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/conversation/{conversation_id}/textdocs",
		}).
		Get(apiPath)
	if err != nil {
		return nil, fmt.Errorf("textdocs 请求失败: %w", err)
	}
	if resp.IsErrorState() {
		return nil, fmt.Errorf("textdocs 返回错误: status=%d body=%s", resp.StatusCode, resp.String()[:min(200, len(resp.String()))])
	}

	// textdocs 返回格式：{"textdocs": [{"type": 0, "thought": {"summary": "...", "content": "...", ...}}, ...]}
	// 或直接是数组
	rawBody := resp.String()
	c.logf("[textdocs] 原始响应 status=%d len=%d snippet=%s", resp.StatusCode, len(rawBody), rawBody[:min(500, len(rawBody))])

	var rawData interface{}
	if err := json.Unmarshal(resp.Bytes(), &rawData); err != nil {
		return nil, fmt.Errorf("textdocs 解析失败: %w", err)
	}

	var chunks []interface{}
	switch v := rawData.(type) {
	case map[string]interface{}:
		// 可能是 {"textdocs": [...]} 或 {"chunks": [...]}
		for _, key := range []string{"textdocs", "chunks", "items", "data"} {
			if arr, ok := v[key].([]interface{}); ok {
				chunks = arr
				break
			}
		}
		if chunks == nil {
			c.logf("[textdocs] 未知顶层结构, keys=%v", mapKeys(v))
		}
	case []interface{}:
		chunks = v
	}

	var steps []ThinkStep
	for _, chunkRaw := range chunks {
		chunk, ok := chunkRaw.(map[string]interface{})
		if !ok {
			continue
		}
		// type=0 是思考段落
		chunkType, _ := chunk["type"].(float64)
		if int(chunkType) != 0 {
			continue
		}
		thought, ok := chunk["thought"].(map[string]interface{})
		if !ok {
			continue
		}
		summary, _ := thought["summary"].(string)
		content, _ := thought["content"].(string)
		if summary == "" && content == "" {
			continue
		}
		steps = append(steps, ThinkStep{
			Summary: summary,
			Content: content,
		})
	}
	return steps, nil
}

// extractThoughts 从 content_type="thoughts" 消息的 thoughts 数组中提取已完成的思考步骤。
// SSE 流中的数组元素格式：{"summary": "...", "content": "...", "chunks": [...], "finished": true}
// 每个 finished=true 的步骤通过 \x00THINK_STEP\x00 标记推送一次（summary\x1Fcontent），去重处理。
func (c *Client) extractThoughts(thoughts []interface{}, result *ChatResult, handler StreamHandler) {
	if result.seenThoughtKeys == nil {
		result.seenThoughtKeys = make(map[string]bool)
	}
	for _, tRaw := range thoughts {
		t, ok := tRaw.(map[string]interface{})
		if !ok {
			continue
		}
		// SSE 格式：直接包含 summary, content, finished
		finished, _ := t["finished"].(bool)
		if !finished {
			continue
		}
		summary, _ := t["summary"].(string)
		content, _ := t["content"].(string)
		if summary == "" {
			continue
		}
		// 去重：同一个 summary 只推送一次
		if result.seenThoughtKeys[summary] {
			continue
		}
		result.seenThoughtKeys[summary] = true
		result.ThinkSteps = append(result.ThinkSteps, ThinkStep{Summary: summary, Content: content})
		c.logf("[thoughts] 新思考步骤: %s", summary)
		if handler != nil {
			payload := summary
			if content != "" {
				payload += "\x1F" + content
			}
			handler("\x00THINK_STEP\x00" + payload)
		}
	}
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
