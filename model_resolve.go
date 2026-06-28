package sentinel

import "strings"

const (
	// ModelDALLE3 ChatGPT 网页生图使用的官方模型（picture_v2 + dall-e-3）。
	ModelDALLE3 = "dall-e-3"

	// 以下为兼容别名，均映射到 dall-e-3（勿与 OpenAI API 的 gpt-image-2 混用）。
	ModelGPTImage2         = "gpt-image-2"
	ModelGPTImage2Thinking = "gpt-image-2-thinking"
)

// ResolvedModel 将 OpenAI 兼容的 model 参数解析为 ChatGPT 后端设置。
type ResolvedModel struct {
	APIModel       string // 回显给客户端的 model
	ChatModel      string // 写入 conversation 请求的 model
	ForcePictureV2 bool   // 是否注入 system_hints picture_v2
}

// ResolveChatModel 解析 model：生图统一走后端 dall-e-3 + picture_v2。
func ResolveChatModel(requestModel string) ResolvedModel {
	m := strings.TrimSpace(requestModel)
	lower := strings.ToLower(m)

	if strings.Contains(lower, "dall-e") {
		return ResolvedModel{
			APIModel:       m,
			ChatModel:      m,
			ForcePictureV2: true,
		}
	}

	switch lower {
	case ModelGPTImage2, ModelGPTImage2Thinking, "gpt-image-2-2026-04-21":
		return ResolvedModel{
			APIModel:       m,
			ChatModel:      ModelDALLE3,
			ForcePictureV2: true,
		}
	}

	if strings.Contains(lower, "gpt-image") {
		return ResolvedModel{
			APIModel:       m,
			ChatModel:      ModelDALLE3,
			ForcePictureV2: true,
		}
	}

	return ResolvedModel{
		APIModel:       m,
		ChatModel:      m,
		ForcePictureV2: false,
	}
}
