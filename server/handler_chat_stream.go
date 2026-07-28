package server

// handler_chat_stream.go —— /v1/chat/completions 的两种响应实现：
//   - handleStream   ：SSE 流式，边生成边下发 delta（含思考、生图/沙箱产物侧信道）；
//   - handleNonStream ：一次性 JSON 响应，产物以 markdown 链接或 sentinel 事件附带。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	sentinel "sentinel-go/sentinel"
)

// handleStream 流式响应
func (h *ChatHandler) handleStream(c *gin.Context, entry *sessionEntry, opts sentinel.ChatOptions, req ChatCompletionRequest, reqConvID, chatID, model string, created int64) {
	includeThinking := req.IncludeThinking || req.PictureV2
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	// 第一个 chunk：role=assistant
	firstSent := false

	w := c.Writer
	flusher, canFlush := w.(http.Flusher)

	writeChunk := func(chunk ChatCompletionChunk) {
		data, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		if canFlush {
			flusher.Flush()
		}
	}

	streamedToClient := strings.Builder{}
	registeredConvID := reqConvID

	writeSentinel := func(ev sentinel.StreamEvent) {
		writeChunk(ChatCompletionChunk{
			ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices:  []ChunkChoice{{Index: 0, Delta: Delta{}, FinishReason: nil}},
			Sentinel: &ev,
		})
	}

	registerSessionForConv := func(convID string) {
		if convID == "" {
			return
		}
		registeredConvID = convID
		h.session.Register(convID, entry)
		opts.Artifacts = h.buildArtifactConfig(c, entry, req, convID, writeSentinel)
	}
	opts.OnConversationID = registerSessionForConv
	registerSessionForConv(reqConvID)
	opts.Artifacts = h.buildArtifactConfig(c, entry, req, registeredConvID, writeSentinel)

	handler := func(delta string) {
		if !includeThinking && len(delta) > 0 && delta[0] == '\x00' {
			return
		}
		if !firstSent {
			// 第一个有内容的 chunk，先发 role
			roleChunk := ChatCompletionChunk{
				ID:      chatID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []ChunkChoice{{
					Index:        0,
					Delta:        Delta{Role: "assistant"},
					FinishReason: nil,
				}},
			}
			writeChunk(roleChunk)
			firstSent = true
		}

		streamedToClient.WriteString(delta)

		contentChunk := ChatCompletionChunk{
			ID:      chatID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []ChunkChoice{{
				Index:        0,
				Delta:        Delta{Content: delta},
				FinishReason: nil,
			}},
		}
		writeChunk(contentChunk)
	}

	result, err := h.chatStreamWithRetry(c, entry, opts, sentinel.StreamHandler(handler))

	if err != nil {
		// 打印详细错误，方便排查 token 问题
		tokenPreview := ""
		if t := entry.token; len(t) > 20 {
			tokenPreview = t[:10] + "..." + t[len(t)-8:]
		} else {
			tokenPreview = entry.token
		}
		fmt.Printf("[chat-err] token=%s error=%v\n", tokenPreview, err)
		errChunk := fmt.Sprintf("data: {\"error\":{\"message\":%q,\"type\":\"server_error\"}}\n\n", err.Error())
		_, _ = io.WriteString(w, errChunk)
		if canFlush {
			flusher.Flush()
		}
		return
	}

	if result.ConversationID != "" {
		registerSessionForConv(result.ConversationID)
	}

	sentinel.LogContentPreview(func(format string, args ...interface{}) {
		fmt.Printf("[chat-stream-client] "+format+"\n", args...)
	}, "stream-deltas", streamedToClient.String())
	sentinel.LogContentPreview(func(format string, args ...interface{}) {
		fmt.Printf("[chat-stream-upstream] "+format+"\n", args...)
	}, "result-text", result.Text)

	// 流式增量未发出/未发全时，用 result.Text 补齐（WS 中断后 conversation 恢复常见）
	streamed := streamedToClient.String()
	if result.Text != "" {
		var missing string
		switch {
		case streamed == "":
			missing = result.Text
		case strings.HasPrefix(result.Text, streamed) && len(result.Text) > len(streamed):
			missing = result.Text[len(streamed):]
		}
		if missing != "" {
			if !firstSent {
				writeChunk(ChatCompletionChunk{
					ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant"}, FinishReason: nil}},
				})
				firstSent = true
			}
			writeChunk(ChatCompletionChunk{
				ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: missing}, FinishReason: nil}},
			})
			streamedToClient.WriteString(missing)
		}
	}

	// 思考步骤详细内容（流结束后推送，仅 Web UI 请求 include_thinking 时）
	if includeThinking && len(result.ThinkSteps) > 0 {
		var thinkContent strings.Builder
		thinkContent.WriteString("\x00THINK_DETAILS\x00")
		for i, step := range result.ThinkSteps {
			if i > 0 {
				thinkContent.WriteString("\x00STEP_SEP\x00")
			}
			thinkContent.WriteString(step.Summary)
			thinkContent.WriteString("\x1F")
			thinkContent.WriteString(step.Content)
		}
		writeChunk(ChatCompletionChunk{
			ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: thinkContent.String()}, FinishReason: nil}},
		})
	}

	if result.ExpectGeneratedImages {
		entry.client.FinishImageGenWS(result, opts)
	}
	// 兜底：沙箱等未在流中推送的产物
	entry.client.EmitNewArtifacts(opts.Artifacts, result)

	fmt.Printf("[chat-done] model=%s conv=%s expect_img=%v image_ids=%v %s text_len=%d streamed=%d\n",
		model, result.ConversationID, result.ExpectGeneratedImages, result.ImageFileIDs,
		result.ImageGenDiagSummary(), len(result.Text), streamedToClient.Len())

	// 兼容：可选 markdown 链接（旧客户端）
	if req.ArtifactMarkdown && result.ExpectGeneratedImages && len(result.ImageFileIDs) > 0 {
		var imgContent strings.Builder
		for i, fileID := range result.ImageFileIDs {
			relURL := fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", registeredConvID, fileID)
			imgContent.WriteString(fmt.Sprintf("\n\n![Generated Image %d](%s)", i+1, buildAbsoluteURL(c, h.cfg, relURL)))
		}
		writeChunk(ChatCompletionChunk{
			ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: imgContent.String()}, FinishReason: nil}},
		})
	} else if req.ArtifactMarkdown && result.ExpectGeneratedImages && result.ImageFileID != "" {
		relURL := fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", registeredConvID, result.ImageFileID)
		writeChunk(ChatCompletionChunk{
			ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: fmt.Sprintf("\n\n![Generated Image](%s)", buildAbsoluteURL(c, h.cfg, relURL))}, FinishReason: nil}},
		})
	} else if req.ArtifactMarkdown && result.ExpectGeneratedImages && result.ImagePath != "" {
		p := result.ImagePath
		if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
			p = strings.ReplaceAll(p, "\\", "/")
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
		}
		writeChunk(ChatCompletionChunk{
			ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: fmt.Sprintf("\n\n![Generated Image](%s)", buildAbsoluteURL(c, h.cfg, p))}, FinishReason: nil}},
		})
	}

	if req.ArtifactMarkdown {
		if files := sandboxFilesForHandler(result); len(files) > 0 {
			var fileContent strings.Builder
			for i, f := range files {
				relURL := fmt.Sprintf("/api/pdf/proxy?conv_id=%s&msg_id=%s&sandbox_path=%s",
					registeredConvID, f.MessageID, url.QueryEscape(f.SandboxPath))
				label := f.FileName
				if label == "" {
					label = fmt.Sprintf("file_%d", i+1)
				}
				fileContent.WriteString(fmt.Sprintf("\n\n[%s](%s)", label, buildAbsoluteURL(c, h.cfg, relURL)))
			}
			writeChunk(ChatCompletionChunk{
				ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: fileContent.String()}, FinishReason: nil}},
			})
		}
	}

	// 最后一个 chunk（stop）
	stopReason := "stop"
	stopChunk := ChatCompletionChunk{
		ID:      chatID,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []ChunkChoice{{
			Index:        0,
			Delta:        Delta{},
			FinishReason: &stopReason,
		}},
		ConversationID: registeredConvID,
	}
	writeChunk(stopChunk)

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if canFlush {
		flusher.Flush()
	}
}

// handleNonStream 非流式响应
func (h *ChatHandler) handleNonStream(c *gin.Context, entry *sessionEntry, opts sentinel.ChatOptions, req ChatCompletionRequest, reqConvID, chatID, model string, created int64) {
	var sentinelEvents []sentinel.StreamEvent
	convForArt := reqConvID
	registerSessionForConv := func(convID string) {
		if convID == "" {
			return
		}
		convForArt = convID
		h.session.Register(convID, entry)
		opts.Artifacts = h.buildArtifactConfig(c, entry, req, convID, func(ev sentinel.StreamEvent) {
			sentinelEvents = append(sentinelEvents, ev)
		})
	}
	opts.OnConversationID = registerSessionForConv
	if reqConvID != "" {
		registerSessionForConv(reqConvID)
	}
	opts.Artifacts = h.buildArtifactConfig(c, entry, req, convForArt, func(ev sentinel.StreamEvent) {
		sentinelEvents = append(sentinelEvents, ev)
	})

	result, err := h.chatWithRetry(c, entry, opts)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: ErrorDetail{Message: err.Error(), Type: "server_error"},
		})
		return
	}

	if result.ConversationID != "" {
		registerSessionForConv(result.ConversationID)
	}

	if result.ExpectGeneratedImages {
		entry.client.FinishImageGenWS(result, opts)
	}
	entry.client.EmitNewArtifacts(opts.Artifacts, result)

	content := result.Text
	sentinel.LogContentPreview(func(format string, args ...interface{}) {
		fmt.Printf("[chat-response] "+format+"\n", args...)
	}, "client-body", content)

	if req.ArtifactMarkdown && result.ExpectGeneratedImages && len(result.ImageFileIDs) > 0 {
		for i, fileID := range result.ImageFileIDs {
			relURL := fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", result.ConversationID, fileID)
			content += fmt.Sprintf("\n\n![Generated Image %d](%s)", i+1, buildAbsoluteURL(c, h.cfg, relURL))
		}
	} else if req.ArtifactMarkdown && result.ExpectGeneratedImages && result.ImageFileID != "" {
		relURL := fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", result.ConversationID, result.ImageFileID)
		content += fmt.Sprintf("\n\n![Generated Image](%s)", buildAbsoluteURL(c, h.cfg, relURL))
	} else if req.ArtifactMarkdown && result.ExpectGeneratedImages && result.ImagePath != "" {
		p := result.ImagePath
		if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
			p = strings.ReplaceAll(p, "\\", "/")
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
		}
		content += fmt.Sprintf("\n\n![Generated Image](%s)", buildAbsoluteURL(c, h.cfg, p))
	}

	if req.ArtifactMarkdown {
		for i, f := range sandboxFilesForHandler(result) {
			relURL := fmt.Sprintf("/api/pdf/proxy?conv_id=%s&msg_id=%s&sandbox_path=%s",
				result.ConversationID, f.MessageID, url.QueryEscape(f.SandboxPath))
			label := f.FileName
			if label == "" {
				label = fmt.Sprintf("file_%d", i+1)
			}
			content += fmt.Sprintf("\n\n[%s](%s)", label, buildAbsoluteURL(c, h.cfg, relURL))
		}
	}

	// 非流式响应：收集思考内容到 reasoning_content
	reasoningContent := ""
	if len(result.ThinkSteps) > 0 {
		var sb strings.Builder
		for i, step := range result.ThinkSteps {
			if i > 0 {
				sb.WriteString("\n\n")
			}
			fmt.Fprintf(&sb, "**%s**\n%s", step.Summary, step.Content)
		}
		reasoningContent = sb.String()
	} else if result.ThinkingText != "" {
		reasoningContent = result.ThinkingText
	}

	resp := ChatCompletionResponse{
		ID:      chatID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []Choice{{
			Index:            0,
			Message:          Message{Role: "assistant", Content: content},
			FinishReason:     "stop",
			ReasoningContent: reasoningContent,
		}},
		Usage:          Usage{},
		ConversationID: result.ConversationID,
		Sentinel:       sentinelEvents,
	}
	c.JSON(http.StatusOK, resp)
}
