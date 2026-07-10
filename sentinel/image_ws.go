package sentinel

// image_ws.go —— 生图在 WebSocket 通道上的异步状态跟踪与 conversation-update 处理。
//
// 网页端生图会通过 WS 推送 async-task-* / set-conversation-async-status 等事件，
// 并在 conversation-update.message 里逐步带出 image_asset_pointer。本文件负责解析这些
// 事件、维护"是否收齐/是否完成"的状态位，并在满足条件时定稿收尾（FinishImageGenWS）。
// 注意：当前收图主路径是 GET /conversation 轮询（见 image_handoff.go / image_revision.go），
// 这里主要覆盖仍走 WS 的旧路径与 WS 断连时的兜底。

import (
	"strings"
	"time"
)

func (c *Client) handleImageWSReadError(err error, result *ChatResult, opts ChatOptions, waitStart time.Time, tag string) (handled bool, retErr error) {
	if err == nil || result == nil {
		return false, nil
	}
	if result.HasDalleGeneratedOutput() {
		c.FinishImageGenWS(result, opts)
		c.logf("[image-ws] WS 断开但已有出图，正常结束 tag=%s err=%v", tag, err)
		return true, nil
	}
	// imagegen skill 路径：WS 断连时若已触发生图工具，尝试 conversation 轮询补全所有图片
	if result.sawImageGenTool && result.ConversationID != "" {
		c.logf("[image-ws] WS 断开（imagegen 路径），切换为 conversation 轮询 tag=%s err=%v", tag, err)
		pollErr := c.pollConversationUntilDone(result, opts)
		c.FinishImageGenWS(result, opts)
		return true, pollErr
	}
	if done, finishErr := c.tryFinishImageGenWS(result, opts, waitStart, tag+"_ws_closed"); done {
		c.logf("[image-ws] WS 关闭后 idle 收齐 tag=%s err=%v", tag, err)
		return true, finishErr
	}
	return false, nil
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

	// thinking 批量生图：图像工具（recipient 如 t2uay3k.sj1i4kz）的工具名不含 dalle/image_gen，
	// 但其 code 节点携带 batch_requests。据内容识别，置 sawImageGenTool 以驱动后续轮询收图。
	if ct, _ := msgContent["content_type"].(string); ct == "code" {
		if txt, _ := msgContent["text"].(string); strings.Contains(txt, "batch_requests") {
			if !result.sawImageGenTool {
				c.logf("[image-route][ws] 检测到 batch_requests code 节点 → 判为生图轮次")
			}
			result.sawImageGenTool = true
			result.ExpectGeneratedImages = true
		}
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
