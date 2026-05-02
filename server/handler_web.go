package server

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed web
var webFS embed.FS

// HandleDashboard 仪表盘页面
func HandleDashboard(c *gin.Context) {
	data, err := webFS.ReadFile("web/dashboard.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "dashboard not found")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

// HandleChatPage 聊天测试页面
func HandleChatPage(c *gin.Context) {
	data, err := webFS.ReadFile("web/chat.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "chat not found")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}
