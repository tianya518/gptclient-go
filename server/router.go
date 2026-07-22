package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewRouter 创建并配置 Gin 路由器
func NewRouter(cfg *ServerConfig, pool *TokenPool, session *SessionManager) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()

	// 中间件：日志 + 恢复 + CORS
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(CORSMiddleware())

	// ─── 公开接口 ──────────────────────────────────────────────────────────────

	// 健康检查
	r.GET("/health", func(c *gin.Context) {
		total, valid, _ := pool.Stats()
		c.JSON(http.StatusOK, gin.H{
			"status":          "ok",
			"tokens_total":    total,
			"tokens_valid":    valid,
			"active_sessions": session.Count(),
		})
	})

	// Token 管理
	tokens := NewTokensHandler(pool, session)
	r.GET("/tokens", tokens.HandleStatus)
	r.POST("/tokens/upload", tokens.HandleUpload)
	r.POST("/tokens/clear", tokens.HandleClear)
	r.GET("/tokens/add/:token", tokens.HandleAddSingle)
	r.GET("/tokens/errors", tokens.HandleErrors)
	r.GET("/tokens/check", tokens.HandleCheck)

	chat := NewChatHandler(cfg, pool, session)

	// ─── 需鉴权接口（OpenAI API）────────────────────────────────────────────────
	apiAuth := r.Group("/")
	apiAuth.Use(AuthMiddleware(cfg, pool))
	{
		apiAuth.POST("/v1/chat/completions", chat.Handle)
		apiAuth.GET("/v1/models", HandleModels)
		apiAuth.POST("/chat/completions", chat.Handle)
		apiAuth.GET("/models", HandleModels)
		// 删除会话（官网软删除：PATCH is_visible=false）
		apiAuth.DELETE("/v1/conversations/:id", chat.HandleDeleteConversation)
		apiAuth.DELETE("/conversations/:id", chat.HandleDeleteConversation)
	}

	// ─── 管理前端与静态资源 ────────────────────────────────────────────────────────
	r.GET("/api/image/proxy", chat.HandleImageProxy)
	r.GET("/api/pdf/proxy", chat.HandlePDFProxy)
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"service": "sentinel-go",
			"message": "OpenAI-compatible API server. Point an OpenAI client (e.g. Open WebUI) at /v1.",
			"endpoints": gin.H{
				"chat":               "/v1/chat/completions",
				"models":             "/v1/models",
				"delete_conversation": "DELETE /v1/conversations/:id",
				"health":             "/health",
			},
		})
	})
	r.Static("/images", cfg.ImageDir)

	return r
}
