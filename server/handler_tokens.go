package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// TokensHandler Token 管理接口处理器
type TokensHandler struct {
	pool    *TokenPool
	session *SessionManager
}

// NewTokensHandler 创建 TokensHandler
func NewTokensHandler(pool *TokenPool, session *SessionManager) *TokensHandler {
	return &TokensHandler{pool: pool, session: session}
}

// HandleStatus 查看 Token 池状态 GET /tokens
func (h *TokensHandler) HandleStatus(c *gin.Context) {
	total, valid, errored := h.pool.Stats()
	c.JSON(http.StatusOK, gin.H{
		"status":          "ok",
		"total":           total,
		"valid":           valid,
		"error":           errored,
		"active_sessions": h.session.Count(),
	})
}

// HandleUpload 上传 Token POST /tokens/upload
// Body: {"tokens": "token1\ntoken2\ntoken3"}
// 或 form: text=token1\ntoken2
func (h *TokensHandler) HandleUpload(c *gin.Context) {
	var body struct {
		Tokens string `json:"tokens" form:"text"`
	}
	// 尝试 JSON 解析，失败则用 form
	if err := c.ShouldBindJSON(&body); err != nil {
		_ = c.ShouldBind(&body)
	}

	if body.Tokens == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "tokens field is required", Type: "invalid_request_error"},
		})
		return
	}

	var validLines []string
	
	// 1. 尝试在整个文本中搜索 JSON 中的 accessToken
	matches := accessTokenRegex.FindAllStringSubmatch(body.Tokens, -1)
	if len(matches) > 0 {
		for _, m := range matches {
			validLines = append(validLines, m[1])
		}
	} else {
		// 2. 如果没有找到，按行分割并过滤明显的垃圾数据
		lines := strings.Split(body.Tokens, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.Contains(line, "{") || strings.Contains(line, "}") {
				continue
			}
			validLines = append(validLines, line)
		}
	}

	added := h.pool.Add(validLines...)

	total, valid, _ := h.pool.Stats()
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"added":        added,
		"tokens_count": valid,
		"total":        total,
	})
}

// HandleAddSingle 添加单个 Token GET /tokens/add/:token
func (h *TokensHandler) HandleAddSingle(c *gin.Context) {
	token := c.Param("token")
	if token == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: ErrorDetail{Message: "token is required", Type: "invalid_request_error"},
		})
		return
	}

	added := h.pool.Add(token)
	total, valid, _ := h.pool.Stats()
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"added":        added,
		"tokens_count": valid,
		"total":        total,
	})
}

// HandleClear 清空所有 Token POST /tokens/clear
func (h *TokensHandler) HandleClear(c *gin.Context) {
	h.pool.Clear()
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"tokens_count": 0,
	})
}

// HandleErrors 查看失效 Token 列表 GET /tokens/errors
func (h *TokensHandler) HandleErrors(c *gin.Context) {
	errTokens := h.pool.ErrorTokens()
	c.JSON(http.StatusOK, gin.H{
		"status":       "success",
		"error_tokens": errTokens,
		"count":        len(errTokens),
	})
}
