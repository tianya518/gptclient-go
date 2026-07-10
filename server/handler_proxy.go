package server

// handler_proxy.go —— 产物下载代理：把 chatgpt.com 的鉴权直链（图片 / Code Interpreter 沙箱文件）
// 通过服务端带 token 代理给客户端，避免前端直连内部地址被 403。

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// HandleImageProxy 处理图片流式代理请求
func (h *ChatHandler) HandleImageProxy(c *gin.Context) {
	convID := c.Query("conv_id")
	fileID := c.Query("file_id")
	if convID == "" || fileID == "" {
		c.String(http.StatusBadRequest, "Missing conv_id or file_id")
		return
	}

	entry, ok := h.session.GetSession(convID)
	if !ok {
		c.String(http.StatusNotFound, "Session not found or expired")
		return
	}

	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}

	err := entry.client.ProxyImageByFileID(fileID, convID, c.Writer, userAgent)
	if err != nil {
		c.String(http.StatusInternalServerError, "Proxy image failed: %v", err)
	}
}

// HandlePDFProxy 代理下载 Code Interpreter 生成的 PDF（及其它沙箱文件）
func (h *ChatHandler) HandlePDFProxy(c *gin.Context) {
	convID := c.Query("conv_id")
	msgID := c.Query("msg_id")
	sandboxPath := c.Query("sandbox_path")
	if convID == "" || msgID == "" || sandboxPath == "" {
		c.String(http.StatusBadRequest, "Missing conv_id, msg_id or sandbox_path")
		return
	}

	entry, ok := h.session.GetSession(convID)
	if !ok {
		c.String(http.StatusNotFound, "Session not found or expired")
		return
	}

	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	}

	if err := entry.client.ProxyPDFBySandboxPath(convID, msgID, sandboxPath, c.Writer, userAgent); err != nil {
		c.String(http.StatusInternalServerError, "Proxy PDF failed: %v", err)
	}
}
