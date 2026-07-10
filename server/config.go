package server

import (
	"os"
	"strconv"
)

// ServerConfig 服务器配置，全部从环境变量读取
type ServerConfig struct {
	// HTTP 服务
	Port string // 监听端口，默认 5005

	// 鉴权：调用本服务的 API Key（区别于 ChatGPT Bearer Token）
	// 若为空，则不校验 Authorization 头（直接将传入的 token 当作 ChatGPT token 使用）
	Authorization string

	// ChatGPT 客户端默认参数
	DefaultModel string // 默认模型，默认 gpt-5-5
	TempMode     bool   // 临时模式（不保存对话历史），默认 false
	ImageDir     string // 图片保存目录，默认 images

	// Token 池
	TokensFile string // Token 持久化文件路径（JSON），默认 tokens.json

	// Session 管理
	SessionTTLMinutes int // Session 不活跃超时（分钟），默认 120

	// 对外地址（可选），用于生成绝对资源链接（图片/PDF 代理 URL）
	// 例如：http://192.168.1.10:5005 或 https://your.domain
	// 若为空，则从请求的 Host / X-Forwarded-Proto 头自动推断
	BaseURL string

	// 出站代理（访问 chatgpt.com），如 socks5://127.0.0.1:10816
	ProxyURL string

	// Token 自动刷新：在 AT 过期前多少秒提前用 ST/RT 换 AT，默认 86400（1 天）
	TokenRefreshAheadSec int

	// 后台定时刷新循环间隔（秒），默认 1800（30 分钟）；<=0 关闭后台循环
	RefreshLoopSec int

	// refresh_token 换 AT 的 OAuth 端点与 client_id（留空用默认 auth.openai.com）
	OAuthTokenURL string
	OAuthClientID string
}

// LoadConfig 从环境变量加载配置
func LoadConfig() ServerConfig {
	return ServerConfig{
		Port:              getEnv("PORT", "5005"),
		Authorization:     getEnv("AUTHORIZATION", ""),
		DefaultModel:      getEnv("DEFAULT_MODEL", "gpt-5-5-thinking"),
		TempMode:          getEnvBool("TEMP_MODE", false),
		ImageDir:          getEnv("IMAGE_DIR", "images"),
		TokensFile:        getEnv("TOKENS_FILE", "tokens.json"),
		SessionTTLMinutes: getEnvInt("SESSION_TTL_MINUTES", 120),
		BaseURL:              getEnv("BASE_URL", ""),
		ProxyURL:             getEnv("PROXY_URL", getEnv("ALL_PROXY", "")),
		TokenRefreshAheadSec: getEnvInt("TOKEN_REFRESH_AHEAD_SEC", 86400),
		RefreshLoopSec:       getEnvInt("REFRESH_LOOP_SEC", 1800),
		OAuthTokenURL:        getEnv("OAUTH_TOKEN_URL", ""),
		OAuthClientID:        getEnv("OAUTH_CLIENT_ID", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func getEnvInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
