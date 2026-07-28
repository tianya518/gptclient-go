package sentinel

// chat_ws.go —— WebSocket 通道相关：连接建立、topic 订阅循环、帧解析与消息分发。
//
// ChatGPT 一轮对话在 SSE 出现 stream_handoff 后，续流可能改走 WebSocket。这里负责：
//   - dialChatWS：拨号并订阅基础 topic；
//   - subscribeWSStream / subscribeWSImageCombined / subscribeWSConvUpdate：三种订阅消费循环；
//   - processWSMessage / processWSEncodedItem / ingestWSMessageObject：把 WS 帧还原为 SSE 事件处理。
// 生图的收图主路径已改为 GET /conversation 轮询（见 image_revision.go），WS 主要承载文本 catchup。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

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
		// imagegen 批量生图路径：图片经 GET /conversation 轮询下发（非 WS）。
		// 用短读超时周期性唤醒循环去驱动顶部的 10s 轮询，避免 ReadMessage 长阻塞饿死轮询。
		if result.sawImageGenTool && !result.imageGenConvAsyncStatusDone && !result.allImageSlotsPopulated() {
			return 2 * time.Second
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
		// imagegen 批量生图路径：勿用 pong 把读 deadline 续到 60s，否则 ReadMessage 永不返回、
		// 顶部 10s 轮询被饿死（图片实际经 GET /conversation 轮询下发）。由循环 readWait 主导。
		if result != nil && result.sawImageGenTool && !result.imageGenConvAsyncStatusDone {
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
		if result.ConversationID != "" {
			shouldFetch := false
			lastPoll := time.Duration(0)
			if result.imageGenConvLastPollAt > 0 {
				lastPoll = time.Since(time.Unix(0, result.imageGenConvLastPollAt))
			}
			if result.sawImageGenTool {
				// imagegen skill 批量生图路径：官网约 10s/次轮询，持续到全部图片就绪或 async_status=4
				if !result.imageGenConvAsyncStatusDone && !result.allImageSlotsPopulated() {
					pollInterval := 10 * time.Second
					if result.imageGenConvPollCount == 0 {
						// 首次：等 SSE turn 结束后 3s 或绝对等待 20s
						if (result.imageGenTurnDone && !turnDoneAt.IsZero() && time.Since(turnDoneAt) >= 3*time.Second) ||
							time.Since(waitStart) >= 20*time.Second {
							shouldFetch = true
						}
					} else if lastPoll >= pollInterval {
						shouldFetch = true
					}
				}
			} else if !result.imageGenConvFetched && !result.HasDalleGeneratedOutput() {
				// 经典 DALL·E fallback：仅一次
				if (result.imageGenTurnDone && !turnDoneAt.IsZero() && time.Since(turnDoneAt) >= 5*time.Second) ||
					time.Since(waitStart) >= 90*time.Second {
					shouldFetch = true
					result.imageGenConvFetched = true
				}
			}
			if shouldFetch {
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
			// 正文已在 SSE/先前帧收齐时，WS 被代理或本机异常关闭不应再判整轮失败。
			if result != nil && !result.ExpectGeneratedImages {
				if strings.TrimSpace(result.assistantFinalText) != "" || result.bodyStreamFromSSE {
					c.logf("[ws] read error after final body ready, treat as done: %v", err)
					return nil
				}
			}
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
							if c.processWSMessage(msg, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent) {
								done = true
							}
						}
					}
				}
				continue
			}

			if fType == "message" {
				frameTopic, _ := frame["topic_id"].(string)
				if frameTopic != topicID {
					continue
				}
				if c.processWSMessage(frame, result, opts, lastText, handler, &useDeltaEncoding, &currentEvent) {
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
