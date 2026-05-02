package server

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// CORSMiddleware 跨域中间件
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, HEAD, PATCH")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, X-Requested-With")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// AuthMiddleware 鉴权中间件
// 若配置了 AUTHORIZATION 环境变量，则验证请求头中的 Bearer Token 是否匹配
// 若未配置，则跳过鉴权（直接将 Bearer Token 视为 ChatGPT token）
func AuthMiddleware(cfg *ServerConfig, pool *TokenPool) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 从请求头提取 Bearer Token（兼容 "Bearer eyJ..." 和 "Bearer" 无空格两种情况）
		auth := c.GetHeader("Authorization")
		// 先去掉 "Bearer "（有空格），再去掉 "Bearer"（无空格），最后 TrimSpace
		token := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(auth, "Bearer "), "Bearer"))
		token = cleanToken(token)

		// 允许“免密模式”或“密码匹配模式”：
		// - 如果传入的 token 就是我们配置的 AUTHORIZATION 密码
		// - 如果传入的 token 为空，且我们没有配置密码（完全开放给本地使用）
		if (cfg.Authorization != "" && token == cfg.Authorization) || (cfg.Authorization == "" && token == "") {
			chatgptToken, ok := pool.Pick()
			if !ok {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, ErrorResponse{
					Error: ErrorDetail{
						Message: "Token pool is empty. Please upload tokens or provide one in the request.",
						Type:    "server_error",
						Code:    "no_token",
					},
				})
				return
			}
			c.Set("chatgpt_token", chatgptToken)
			c.Set("from_pool", true)
		} else if cfg.Authorization != "" && token != "" {
			// 如果配置了密码，且传入了密码，但不匹配
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
				Error: ErrorDetail{
					Message: "Invalid API key",
					Type:    "invalid_request_error",
					Code:    "invalid_api_key",
				},
			})
			return
		} else if token == "" {
			// 这种情况只有一种：配了密码，但是 token 为空，此时应该提示需要鉴权
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{
				Error: ErrorDetail{
					Message: "Missing Authorization header",
					Type:    "invalid_request_error",
					Code:    "missing_auth",
				},
			})
			return
		} else {
			// 未配置 AUTHORIZATION，且传入了 token，直接将 token 作为 ChatGPT Bearer token 透传
			c.Set("chatgpt_token", token)
			c.Set("from_pool", false)
		}

		c.Next()
	}
}

// extractChatGPTToken 从 gin Context 中取出 chatgpt_token
func extractChatGPTToken(c *gin.Context) string {
	v, _ := c.Get("chatgpt_token")
	t, _ := v.(string)
	return t
}
