package sentinel

import (
	"context"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

const (
	defaultUA          = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 Edg/147.0.0.0"
	defaultBuildHash   = "prod-81e0c5cdf6140e8c5db714d613337f4aeab94029"
	defaultBuildNumber = "6128297"
	defaultLang        = "zh-CN"
	defaultModel       = "gpt-5-5-thinking"
)

// Client 是 ChatGPT 对话客户端，封装了完整的 Sentinel 认证 + SSE 对话流程。
type Client struct {
	httpClient  *req.Client
	bearerToken string
	cookieStr   string
	userAgent   string
	deviceID    string
	buildHash   string
	buildNumber string
	language    string
	sessionID   string
	imageDir    string
	startTime   time.Time

	conversationID  string
	parentMessageID string
	model           string
	tempMode        bool
	turnCount       int

	// Logf 日志输出函数，设为 nil 可禁用日志。默认 log.Printf。
	Logf LogFunc

	// DisableAutoImage 设为 true 时，Chat/ChatStream 不会自动阻塞等待图片下载。
	// 适合 DLL / 外部调用场景，由调用方自己异步处理图片下载。
	DisableAutoImage bool

	// StreamRecorder 非空时记录全部 SSE 事件（供 stream-capture 分析）。
	StreamRecorder *StreamRecorder

	proxyURL string // 出站代理（HTTP + WebSocket）
}

// NewClient 创建新的 ChatGPT 客户端
func NewClient(cfg Config) *Client {
	c := &Client{
		bearerToken:     cfg.BearerToken,
		cookieStr:       cfg.CookieString,
		userAgent:       orDefault(cfg.UserAgent, defaultUA),
		deviceID:        orDefault(cfg.DeviceID, GenerateUUID()),
		buildHash:       orDefault(cfg.BuildHash, defaultBuildHash),
		buildNumber:     orDefault(cfg.BuildNumber, defaultBuildNumber),
		language:        orDefault(cfg.Language, defaultLang),
		imageDir:        orDefault(cfg.ImageDir, "images"),
		model:           orDefault(cfg.Model, defaultModel),
		parentMessageID: "client-created-root",
		sessionID:       GenerateUUID(),
		startTime:       time.Now(),
		tempMode:        cfg.TempMode,
		Logf:            log.Printf,
	}

	httpC := req.C().
		SetBaseURL("https://chatgpt.com").
		SetCommonHeaders(c.commonHeaders()).
		ImpersonateChrome()
	httpC.SetTLSHandshakeTimeout(20 * time.Second)

	// 默认强制 IPv4 拨号：隧道/代理环境下 chatgpt.com 的 IPv6 路径常出现 TLS 握手卡死，
	// 而纯 Go 的 utls 传输不会像 libcurl 那样在超时内回退到 IPv4。设 SENTINEL_ALLOW_IPV6=1 恢复双栈。
	if os.Getenv("SENTINEL_ALLOW_IPV6") == "" {
		dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		httpC.SetDial(func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		})
	}

	if proxyURL := strings.TrimSpace(cfg.ProxyURL); proxyURL != "" {
		c.proxyURL = proxyURL
		httpC.SetProxyURL(proxyURL)
		if c.Logf != nil {
			c.Logf("[proxy] 出站代理: %s", proxyURL)
		}
	}

	c.httpClient = httpC
	return c
}

// HTTPClient 返回底层 req.Client 以便高级自定义
func (c *Client) HTTPClient() *req.Client {
	return c.httpClient
}

// ResetSession 重置对话上下文（开始新对话）
func (c *Client) ResetSession() {
	c.conversationID = ""
	c.parentMessageID = "client-created-root"
	c.turnCount = 0
}

// SetModel 切换模型
func (c *Client) SetModel(model string) { c.model = model }

// GetModel 获取当前模型
func (c *Client) GetModel() string { return c.model }

// SetTempMode 设置临时模式
func (c *Client) SetTempMode(enabled bool) { c.tempMode = enabled }

// SetDisableAutoImage 设置是否禁用自动图片下载（DLL 场景使用）
func (c *Client) SetDisableAutoImage(disabled bool) { c.DisableAutoImage = disabled }

// SetBearerToken 更新 Bearer Token（Session Token 刷新后调用）。
func (c *Client) SetBearerToken(token string) {
	c.bearerToken = token
	c.httpClient.SetCommonHeader("Authorization", "Bearer "+token)
}

// SetConversationID 恢复到指定对话
func (c *Client) SetConversationID(id string) { c.conversationID = id }

// SetParentMessageID 设置父消息 ID（用于指定回复位置）
func (c *Client) SetParentMessageID(id string) { c.parentMessageID = id }

// GetSessionInfo 获取当前会话状态
func (c *Client) GetSessionInfo() SessionInfo {
	return SessionInfo{
		ConversationID:  c.conversationID,
		ParentMessageID: c.parentMessageID,
		Model:           c.model,
		TempMode:        c.tempMode,
		TurnCount:       c.turnCount,
	}
}

func (c *Client) logf(format string, args ...interface{}) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) commonHeaders() map[string]string {
	h := map[string]string{
		"Authorization":               "Bearer " + c.bearerToken,
		"User-Agent":                  c.userAgent,
		"Accept-Language":             c.language + ",zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6",
		"oai-language":                c.language,
		"oai-device-id":               c.deviceID,
		"oai-session-id":              c.sessionID,
		"oai-client-version":          c.buildHash,
		"oai-client-build-number":     c.buildNumber,
		"Origin":                      "https://chatgpt.com",
		"Referer":                     "https://chatgpt.com/",
		"sec-ch-ua":                   `"Chromium";v="146", "Not-A.Brand";v="24", "Microsoft Edge";v="146"`,
		"sec-ch-ua-mobile":            "?0",
		"sec-ch-ua-platform":          `"Windows"`,
		"sec-ch-ua-platform-version":  `"19.0.0"`,
		"sec-ch-ua-arch":              `"x86"`,
		"sec-ch-ua-bitness":           `"64"`,
		"sec-ch-ua-model":             `""`,
		"sec-ch-ua-full-version":      `"146.0.3856.72"`,
		"sec-ch-ua-full-version-list": `"Chromium";v="146.0.7680.154", "Not-A.Brand";v="24.0.0.0", "Microsoft Edge";v="146.0.3856.72"`,
		"sec-fetch-dest":              "empty",
		"sec-fetch-mode":              "cors",
		"sec-fetch-site":              "same-origin",
	}
	if c.cookieStr != "" {
		h["Cookie"] = c.cookieStr
	}
	return h
}
