package server

import (
	"time"

	sentinel "sentinel-go"
)

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

	// PictureV2 为 true 时注入 system_hints picture_v2 强制走 DALL·E 生图（可与任意 model 同用，便于续接 conversation_id）
	PictureV2 bool `json:"picture_v2,omitempty"`

	// 图片生成专用参数（model 为 dall-e* 时也会自动启用 picture_v2；gpt-image* 兼容别名会映射为 dall-e-3）
	// size 接受宽高比字符串：1:1 / 3:4 / 9:16 / 4:3 / 16:9
	// 也兼容 OpenAI 格式：256x256 / 512x512 / 1024x1024 / 1792x1024 / 1024x1792
	Size string `json:"size,omitempty"`
	// n 生成张数（暂不支持 >1，预留字段）
	N int `json:"n,omitempty"`

	// IncludeThinking 为 true 时流式响应包含 \x00THINK\x00 思考增量（内置 Web UI 使用）；默认 false，仅输出 final 正文
	IncludeThinking bool `json:"include_thinking,omitempty"`

	// ArtifactDelivery 产物下发：url（默认）| base64 | base64_chunked
	ArtifactDelivery string `json:"artifact_delivery,omitempty"`
	// ArtifactBase64ChunkSize base64_chunked 时每块原始字节数，默认 393216
	ArtifactBase64ChunkSize int `json:"artifact_base64_chunk_size,omitempty"`
	// ArtifactMarkdown 为 true 时在 content 末尾附加 markdown 链接（兼容旧客户端）；默认 false
	ArtifactMarkdown bool `json:"artifact_markdown,omitempty"`
	// ArtifactImageRevisions 生图多版本：all | latest_per_slot（默认）| final_only
	ArtifactImageRevisions string `json:"artifact_image_revisions,omitempty"`
}

// Message OpenAI 消息格式
type Message struct {
	Role    string      `json:"role"`    // "system" | "user" | "assistant"
	Content interface{} `json:"content"` // 文本内容 (string) 或 多模态数组 ([]ContentPart)
}

// ContentPart 多模态内容项
type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url" | "file"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	File     *FilePart `json:"file,omitempty"` // type=file 时使用
}

// ImageURL 图片链接或 Base64
type ImageURL struct {
	URL string `json:"url"` // 形如 "data:image/jpeg;base64,..." 或普通 URL
}

// FilePart 文件类型内容（对应 OpenAI type=file）
type FilePart struct {
	FileID   string `json:"file_id,omitempty"`   // 预上传的 file_id 引用
	Filename string `json:"filename,omitempty"`  // 文件名（配合 file_data 使用）
	FileData string `json:"file_data,omitempty"` // base64 编码的文件内容（data: URL 格式）
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

	// Sentinel 侧信道事件（非流式一次返回数组）
	Sentinel []sentinel.StreamEvent `json:"sentinel,omitempty"`
}

// Choice 非流式选项
type Choice struct {
	Index            int     `json:"index"`
	Message          Message `json:"message"`
	FinishReason     string  `json:"finish_reason"`
	ReasoningContent string  `json:"reasoning_content,omitempty"` // 思考内容（兼容 DeepSeek 风格）
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

	// Sentinel 侧信道：产物/进度（与 delta.content 正文分离）
	Sentinel *sentinel.StreamEvent `json:"sentinel,omitempty"`
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
