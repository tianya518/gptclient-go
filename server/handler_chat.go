package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	sentinel "sentinel-go"
)

// ChatHandler 持有依赖，负责 /v1/chat/completions 路由
type ChatHandler struct {
	cfg     *ServerConfig
	pool    *TokenPool
	session *SessionManager
}

// NewChatHandler 创建 ChatHandler
func NewChatHandler(cfg *ServerConfig, pool *TokenPool, session *SessionManager) *ChatHandler {
	return &ChatHandler{cfg: cfg, pool: pool, session: session}
}

// Handle 处理 POST /v1/chat/completions
func (h *ChatHandler) Handle(c *gin.Context) {
	var req ChatCompletionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "Invalid JSON body", Type: "invalid_request_error"},
		})
		return
	}

	if req.Model == "" {
		req.Model = h.cfg.DefaultModel
	}

	// 获取当前请求使用的 ChatGPT token（由鉴权中间件写入）
	token := extractChatGPTToken(c)

	// 提取最后一条 user 消息作为本轮输入
	userMsg, systemPrompt, b64Images := extractUserMessage(req.Messages)
	if userMsg == "" && len(b64Images) == 0 {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "No user message or images found in messages", Type: "invalid_request_error"},
		})
		return
	}

	// 获取或创建 session（有状态多轮对话）
	entry := h.session.GetOrCreate(req.ConversationID, token)
	if req.ConversationID != "" {
		h.session.Register(req.ConversationID, entry)
	}

	// 如果有 system prompt 且是新对话（无 conversationID），拼接到用户消息前面
	inputMsg := userMsg
	if systemPrompt != "" && req.ConversationID == "" && entry.client.GetModel() != "" {
		inputMsg = "[System]: " + systemPrompt + "\n\n" + userMsg
	}

	// 处理文件上传（图片 + 文档 + 其他类型）
	var uploadedImages []sentinel.UploadedFile
	for _, b64 := range b64Images {
		var data []byte
		var fileName, mimeHint string
		var err error

		if strings.HasPrefix(b64, "http://") || strings.HasPrefix(b64, "https://") {
			// HTTP/HTTPS URL：先下载再上传
			data, fileName, mimeHint, err = downloadURL(b64)
			if err != nil || len(data) == 0 {
				continue
			}
		} else if strings.HasPrefix(b64, "data:") {
			// 解析 data URL：data:<mime>;base64,<data>  或  data:<mime>,<data>
			commaIdx := strings.Index(b64, ",")
			if commaIdx < 0 {
				continue
			}
			header := b64[5:commaIdx]   // e.g. "application/pdf;base64" or "image/jpeg;base64"
			payload := b64[commaIdx+1:] // base64 encoded data

			if strings.Contains(header, ";base64") {
				data, err = base64.StdEncoding.DecodeString(payload)
			} else {
				data = []byte(payload)
			}
			if err != nil || len(data) == 0 {
				continue
			}
			mimeHint = strings.TrimSuffix(header, ";base64")
			fileName = guessFileName(mimeHint)
		} else {
			continue
		}

		uf, err := entry.client.UploadFile(c.Request.Context(), data, fileName, mimeHint)
		if err == nil && uf != nil {
			uploadedImages = append(uploadedImages, *uf)
		}
	}

	resolved := sentinel.ResolveChatModel(req.Model)
	apiModel := resolved.APIModel
	if apiModel == "" {
		apiModel = req.Model
	}

	// 切换模型（生图别名会映射为 dall-e-3）
	if resolved.ChatModel != "" && resolved.ChatModel != entry.client.GetModel() {
		entry.client.SetModel(resolved.ChatModel)
	}

	opts := sentinel.ChatOptions{
		Text:           inputMsg,
		Images:         uploadedImages,
		ForcePictureV2: resolved.ForcePictureV2 || req.PictureV2,
		ImageAspect:    sizeToAspect(req.Size),
	}

	chatID := "chatcmpl-" + sentinel.GenerateUUID()
	createdAt := time.Now().Unix()

	if req.Stream {
		h.handleStream(c, entry, opts, req, req.ConversationID, chatID, apiModel, createdAt)
	} else {
		h.handleNonStream(c, entry, opts, req, req.ConversationID, chatID, apiModel, createdAt)
	}
}

func (h *ChatHandler) buildArtifactConfig(c *gin.Context, entry *sessionEntry, req ChatCompletionRequest, convID string, onEvent func(sentinel.StreamEvent)) sentinel.ArtifactStreamConfig {
	return sentinel.ArtifactStreamConfig{
		Delivery:         req.ArtifactDelivery,
		ChunkSize:        req.ArtifactBase64ChunkSize,
		ImageRevisions:   req.ArtifactImageRevisions,
		OnEvent:          onEvent,
		BuildImageURL: func(fileID string) string {
			cid := convID
			if cid == "" && entry != nil {
				cid = entry.client.GetSessionInfo().ConversationID
			}
			rel := fmt.Sprintf("/api/image/proxy?conv_id=%s&file_id=%s", cid, fileID)
			return buildAbsoluteURL(c, h.cfg, rel)
		},
		BuildSandboxURL: func(messageID, sandboxPath string) string {
			rel := fmt.Sprintf("/api/pdf/proxy?conv_id=%s&msg_id=%s&sandbox_path=%s",
				convID, messageID, url.QueryEscape(sandboxPath))
			return buildAbsoluteURL(c, h.cfg, rel)
		},
	}
}

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
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{}, FinishReason: nil}},
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

	// 流式增量未发出但 result.Text 已有正文（例如仅在 WS catchup 收齐）时补发
	if streamedToClient.Len() == 0 && result.Text != "" {
		if !firstSent {
			writeChunk(ChatCompletionChunk{
				ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
				Choices: []ChunkChoice{{Index: 0, Delta: Delta{Role: "assistant"}, FinishReason: nil}},
			})
			firstSent = true
		}
		writeChunk(ChatCompletionChunk{
			ID: chatID, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: result.Text}, FinishReason: nil}},
		})
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

// parseMessageContent 解析多模态内容或纯文本内容
func parseMessageContent(c interface{}) (text string, images []string) {
	if c == nil {
		return
	}
	if s, ok := c.(string); ok {
		return s, nil
	}
	if arr, ok := c.([]interface{}); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				t, _ := m["type"].(string)
				if t == "text" {
					if txt, ok := m["text"].(string); ok {
						text += txt
					}
			} else if t == "image_url" {
				if imgUrl, ok := m["image_url"].(map[string]interface{}); ok {
					if url, ok := imgUrl["url"].(string); ok {
						images = append(images, url)
					}
				}
			} else if t == "file" {
				if filePart, ok := m["file"].(map[string]interface{}); ok {
					if fileData, ok := filePart["file_data"].(string); ok && fileData != "" {
						// data:application/pdf;base64,... 格式，直接复用 data URL 通道
						images = append(images, fileData)
					}
				}
			}
			}
		}
	}
	return
}

// extractUserMessage 从 messages 中提取最后一条 user 消息和 system 提示词
func extractUserMessage(messages []Message) (userMsg string, systemPrompt string, images []string) {
	// 找 system prompt
	for _, m := range messages {
		if strings.ToLower(m.Role) == "system" {
			systemPrompt, _ = parseMessageContent(m.Content)
			break
		}
	}
	// 找最后一条 user 消息
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.ToLower(messages[i].Role) == "user" {
			userMsg, images = parseMessageContent(messages[i].Content)
			break
		}
	}
	return
}

// HandleImageProxy 处理图片流式代理请求
func (h *ChatHandler) HandleImageProxy(c *gin.Context) {
	convID := c.Query("conv_id")
	fileID := c.Query("file_id")
	if convID == "" || fileID == "" {
		c.String(http.StatusBadRequest, "Missing conv_id or file_id")
		return
	}

	entry, ok := h.session.GetSession(convID)
	if !ok {
		c.String(http.StatusNotFound, "Session not found or expired")
		return
	}

	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}

	err := entry.client.ProxyImageByFileID(fileID, convID, c.Writer, userAgent)
	if err != nil {
		c.String(http.StatusInternalServerError, "Proxy image failed: %v", err)
	}
}

// HandlePDFProxy 代理下载 Code Interpreter 生成的 PDF
func (h *ChatHandler) HandlePDFProxy(c *gin.Context) {
	convID := c.Query("conv_id")
	msgID := c.Query("msg_id")
	sandboxPath := c.Query("sandbox_path")
	if convID == "" || msgID == "" || sandboxPath == "" {
		c.String(http.StatusBadRequest, "Missing conv_id, msg_id or sandbox_path")
		return
	}

	entry, ok := h.session.GetSession(convID)
	if !ok {
		c.String(http.StatusNotFound, "Session not found or expired")
		return
	}

	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}

	if err := entry.client.ProxyPDFBySandboxPath(convID, msgID, sandboxPath, c.Writer, userAgent); err != nil {
		c.String(http.StatusInternalServerError, "Proxy PDF failed: %v", err)
	}
}

// guessFileName 根据 MIME 类型猜测一个合适的文件名
func guessFileName(mime string) string {
	extMap := map[string]string{
		"image/jpeg":                                                          "upload.jpg",
		"image/png":                                                           "upload.png",
		"image/gif":                                                           "upload.gif",
		"image/webp":                                                          "upload.webp",
		"application/pdf":                                                     "document.pdf",
		"application/msword":                                                  "document.doc",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "document.docx",
		"application/vnd.ms-excel":                                           "spreadsheet.xls",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":  "spreadsheet.xlsx",
		"application/vnd.ms-powerpoint":                                      "presentation.ppt",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": "presentation.pptx",
		"text/plain":                                                          "document.txt",
		"text/csv":                                                            "data.csv",
		"application/json":                                                    "data.json",
		"text/markdown":                                                       "document.md",
	}
	if name, ok := extMap[mime]; ok {
		return name
	}
	return "file"
}

// downloadURL 下载 HTTP/HTTPS URL 的内容，返回字节数据、文件名与 Content-Type（作 mimeHint）
func downloadURL(rawURL string) ([]byte, string, string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("download %s: HTTP %d", rawURL, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read body %s: %w", rawURL, err)
	}

	// 从 Content-Type 推断 MIME 与文件名
	contentType := resp.Header.Get("Content-Type")
	mimeType := strings.Split(contentType, ";")[0]
	mimeType = strings.TrimSpace(mimeType)
	fileName := guessFileName(mimeType)

	// 如果 URL 末尾有文件名，也可以用它
	if fileName == "file" {
		if idx := strings.LastIndex(rawURL, "/"); idx >= 0 {
			candidate := rawURL[idx+1:]
			// 去掉 query string
			if qIdx := strings.Index(candidate, "?"); qIdx >= 0 {
				candidate = candidate[:qIdx]
			}
			if strings.Contains(candidate, ".") {
				fileName = candidate
			}
		}
	}
	return data, fileName, mimeType, nil
}

// buildAbsoluteURL 将相对路径转换为绝对 URL
// 优先使用 cfg.BaseURL，其次从请求头推断
func buildAbsoluteURL(c *gin.Context, cfg *ServerConfig, path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if cfg.BaseURL != "" {
		base := strings.TrimRight(cfg.BaseURL, "/")
		return base + path
	}
	scheme := "http"
	if c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := c.Request.Host
	return scheme + "://" + host + path
}

// sizeToAspect 将 OpenAI 风格的 size 字符串转换为 ImageAspectRatio。
// 支持 "1:1" / "3:4" / "9:16" / "4:3" / "16:9" 宽高比直写，
// 以及兼容 OpenAI 像素格式 "256x256" / "1024x1024" / "1792x1024" / "1024x1792"。
func sandboxFilesForHandler(result *sentinel.ChatResult) []sentinel.SandboxArtifact {
	if len(result.SandboxArtifacts) > 0 {
		return result.SandboxArtifacts
	}
	out := make([]sentinel.SandboxArtifact, len(result.PDFArtifacts))
	for i, p := range result.PDFArtifacts {
		out[i] = sentinel.SandboxArtifact(p)
	}
	return out
}

func sizeToAspect(size string) sentinel.ImageAspectRatio {
	switch strings.TrimSpace(strings.ToLower(size)) {
	case "1:1", "256x256", "512x512", "1024x1024":
		return sentinel.ImageAspectSquare
	case "3:4":
		return sentinel.ImageAspectPortrait
	case "9:16", "1024x1792":
		return sentinel.ImageAspectStory
	case "4:3":
		return sentinel.ImageAspectLandscape
	case "16:9", "1792x1024":
		return sentinel.ImageAspectWidescreen
	default:
		return sentinel.ImageAspectAuto
	}
}
