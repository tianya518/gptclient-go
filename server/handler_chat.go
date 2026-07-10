package server

// handler_chat.go —— /v1/chat/completions 入口与请求编排。
//
// Handle 负责：解析请求、取本轮 token/session、上传附件、解析模型别名，组装 sentinel.ChatOptions，
// 再按 stream 分流到 handleStream / handleNonStream（见 handler_chat_stream.go）。
// 其它职责拆到同包文件：
//   - handler_chat_input.go：messages 内容解析
//   - handler_proxy.go      ：图片 / PDF 下载代理
//   - handler_util.go       ：文件名猜测、URL 下载、绝对 URL、size 换算等
//   - handler_auth_retry.go ：chatWithRetry / chatStreamWithRetry（鉴权失败自动换 token 重试）

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	sentinel "sentinel-go/sentinel"
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
		// ThinkingEffort 由模型解析表确定（空串 = 不携带字段，对应极速/o3 等）
		ThinkingEffort: resolved.ThinkingEffort,
	}

	chatID := "chatcmpl-" + sentinel.GenerateUUID()
	createdAt := time.Now().Unix()

	if req.Stream {
		h.handleStream(c, entry, opts, req, req.ConversationID, chatID, apiModel, createdAt)
	} else {
		h.handleNonStream(c, entry, opts, req, req.ConversationID, chatID, apiModel, createdAt)
	}
}

// buildArtifactConfig 组装产物流式配置（生图/沙箱文件的代理 URL 构造与事件回调）。
func (h *ChatHandler) buildArtifactConfig(c *gin.Context, entry *sessionEntry, req ChatCompletionRequest, convID string, onEvent func(sentinel.StreamEvent)) sentinel.ArtifactStreamConfig {
	return sentinel.ArtifactStreamConfig{
		Delivery:       req.ArtifactDelivery,
		ChunkSize:      req.ArtifactBase64ChunkSize,
		ImageRevisions: req.ArtifactImageRevisions,
		OnEvent:        onEvent,
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
