package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	sentinel "sentinel-go/sentinel"
)

// HandleDeleteConversation 删除（隐藏）指定会话。
//
//	DELETE /v1/conversations/:id
//	DELETE /conversations/:id
//
// 对齐官网：PATCH /backend-api/conversation/{id} + {"is_visible":false}
func (h *ChatHandler) HandleDeleteConversation(c *gin.Context) {
	convID := strings.TrimSpace(c.Param("id"))
	if convID == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "conversation id is required", Type: "invalid_request_error"},
		})
		return
	}

	token := extractChatGPTToken(c)
	if token == "" {
		c.JSON(http.StatusUnauthorized, ErrorResponse{
			Error: ErrorDetail{Message: "missing ChatGPT token", Type: "authentication_error"},
		})
		return
	}

	// 优先复用已绑定该 conversation 的 session client（带同一 proxy/headers）
	var client *sentinel.Client
	if entry, ok := h.session.GetSession(convID); ok && entry != nil && entry.client != nil {
		client = entry.client
		if token != "" && entry.token != token {
			client.SetBearerToken(token)
		}
	} else {
		client = sentinel.NewClient(sentinel.Config{
			BearerToken: token,
			Model:       h.cfg.DefaultModel,
			TempMode:    h.cfg.TempMode,
			ImageDir:    h.cfg.ImageDir,
			ProxyURL:    h.cfg.ProxyURL,
		})
	}

	if err := client.DeleteConversation(convID); err != nil {
		c.JSON(http.StatusBadGateway, ErrorResponse{
			Error: ErrorDetail{Message: err.Error(), Type: "upstream_error"},
		})
		return
	}

	// 同步清掉本地 session 缓存
	h.session.Delete(convID)

	c.JSON(http.StatusOK, gin.H{
		"success":         true,
		"conversation_id": convID,
		"is_visible":      false,
	})
}
