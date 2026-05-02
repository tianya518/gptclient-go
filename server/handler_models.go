package server

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// supportedModels 支持的模型列表（ChatGPT Plus 当前可用）
var supportedModels = []Model{
	// ── GPT 系列 ────────────────────────────────────────────────
	{ID: "gpt-5-5-thinking", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-5", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-4o", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "gpt-4o-mini", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	// ── o 推理系列 ───────────────────────────────────────────────
	{ID: "o3", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "o4-mini", Object: "model", Created: 1700000000, OwnedBy: "openai"},
	{ID: "o4-mini-high", Object: "model", Created: 1700000000, OwnedBy: "openai"},
}

func init() {
	// 用当前时间填充 Created 字段（更真实）
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
