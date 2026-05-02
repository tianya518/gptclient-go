package server

import "time"

// ─── 请求类型 ────────────────────────────────────────────────────────────────

// ChatCompletionRequest OpenAI 格式的对话请求
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`

	// 可选标准字段（我们目前不处理，仅透传给客户端记录）
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`

	// 扩展字段：conversation_id，用于有状态多轮对话
	// 首次请求无需传入，响应中会返回，下次请求带上即可续接上下文
	ConversationID string `json:"conversation_id,omitempty"`
}

// Message OpenAI 消息格式
type Message struct {
	Role    string      `json:"role"`    // "system" | "user" | "assistant"
	Content interface{} `json:"content"` // 文本内容 (string) 或 多模态数组 ([]ContentPart)
}

// ContentPart 多模态内容项
type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL 图片链接或 Base64
type ImageURL struct {
	URL string `json:"url"` // 形如 "data:image/jpeg;base64,..." 或普通 URL
}

// ─── 非流式响应 ───────────────────────────────────────────────────────────────

// ChatCompletionResponse 非流式响应（OpenAI 格式）
type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`

	// 扩展字段：返回给客户端，下次请求带上即可续接对话
	ConversationID string `json:"conversation_id,omitempty"`
}

// Choice 非流式选项
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage token 用量（ChatGPT 逆向无法精确统计，用 0 占位）
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ─── 流式响应（SSE） ──────────────────────────────────────────────────────────

// ChatCompletionChunk SSE 流式 chunk（OpenAI 格式）
type ChatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`

	// 扩展字段：只在第一个 chunk 中返回
	ConversationID string `json:"conversation_id,omitempty"`
}

// ChunkChoice 流式选项
type ChunkChoice struct {
	Index        int       `json:"index"`
	Delta        Delta     `json:"delta"`
	FinishReason *string   `json:"finish_reason"` // 最后一个 chunk 为 "stop"，其余为 null
	Logprobs     *struct{} `json:"logprobs"`
}

// Delta 流式增量内容
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// ─── 模型列表 ─────────────────────────────────────────────────────────────────

// ModelList /v1/models 响应
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Model 单个模型信息
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ─── 错误响应 ─────────────────────────────────────────────────────────────────

// ErrorResponse OpenAI 格式的错误响应
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 错误详情
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// ─── Token 管理 ───────────────────────────────────────────────────────────────

// TokensStatus Token 池状态
type TokensStatus struct {
	Status      string `json:"status"`
	TokensCount int    `json:"tokens_count"`
	ErrorCount  int    `json:"error_count,omitempty"`
}

// ─── 辅助函数 ─────────────────────────────────────────────────────────────────

func nowUnix() int64 {
	return time.Now().Unix()
}

func strPtr(s string) *string {
	return &s
}
