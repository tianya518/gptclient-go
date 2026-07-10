package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// supportedModels 当前对外暴露的模型列表（2026-07-09 MCP 抓包更新）。
//
// 官网 UI → 后端 model + thinking_effort 完整映射（顶层3档 × 模型族）：
//
//	UI 显示                请求 model（用户传入）   后端 model            thinking_effort
//	极速 5.3               gpt-5-3-instant          gpt-5-3-instant      （不携带）
//	均衡 / GPT-5.5         gpt-5-5-thinking          gpt-5-5-thinking     standard
//	高级（默认）           gpt-5-5-thinking + ext    gpt-5-5-thinking     extended
//	5.4 均衡               gpt-5-4                   gpt-5-4-thinking     standard
//	5.4 高级               gpt-5-4-thinking          gpt-5-4-thinking     extended
//	o3 / Medium            o3                        o3                   （不携带）
//	图片生成               dall-e-3                  dall-e-3             n/a
//
// 注：官网"极速档"无论在 5.5/5.4 模型族下均使用 gpt-5-3-instant，没有独立的 gpt-5-4 极速 model。
var supportedModels = []Model{
	// 通用聊天模型（按推荐顺序）
	{ID: "gpt-5-5-thinking", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-5-5", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-5-4-thinking", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-5-3-instant", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "o3", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	// 图片生成（需配合 picture_v2；gpt-image-2 等别名会自动映射到此）
	{ID: "dall-e-3", Object: "model", Created: 1700000000, OwnedBy: "openai"},
}

func init() {
	ts := time.Now().Unix()
	for i := range supportedModels {
		supportedModels[i].Created = ts
	}
}

// HandleModels 处理 GET /v1/models
func HandleModels(c *gin.Context) {
	c.JSON(http.StatusOK, ModelList{
		Object: "list",
		Data:   supportedModels,
	})
}
