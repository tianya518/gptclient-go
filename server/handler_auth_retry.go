package server

import (
	"strings"

	"github.com/gin-gonic/gin"

	sentinel "sentinel-go/sentinel"
)

func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") ||
		strings.Contains(s, "unauthorized") ||
		strings.Contains(s, "invalid_token") ||
		strings.Contains(s, "token expired")
}

func fromTokenPool(c *gin.Context) bool {
	v, ok := c.Get("from_pool")
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func (h *ChatHandler) chatStreamWithRetry(
	c *gin.Context,
	entry *sessionEntry,
	opts sentinel.ChatOptions,
	handler sentinel.StreamHandler,
) (*sentinel.ChatResult, error) {
	result, err := entry.client.ChatStream(opts, handler)
	if err == nil || !isAuthError(err) || !fromTokenPool(c) || h.pool == nil {
		return result, err
	}
	return h.retryAfterRefresh(entry, opts, handler, err)
}

func (h *ChatHandler) chatWithRetry(c *gin.Context, entry *sessionEntry, opts sentinel.ChatOptions) (*sentinel.ChatResult, error) {
	return h.chatStreamWithRetry(c, entry, opts, nil)
}

func (h *ChatHandler) retryAfterRefresh(
	entry *sessionEntry,
	opts sentinel.ChatOptions,
	handler sentinel.StreamHandler,
	firstErr error,
) (*sentinel.ChatResult, error) {
	oldAT := entry.token
	newAT, ok := h.pool.TryRefreshAT(oldAT)
	if !ok {
		h.pool.MarkError(oldAT)
		return nil, firstErr
	}
	entry.client.SetBearerToken(newAT)
	entry.token = newAT
	return entry.client.ChatStream(opts, handler)
}
