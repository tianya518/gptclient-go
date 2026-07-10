package server

// handler_util.go —— ChatHandler 的通用小工具：MIME→文件名猜测、URL 下载、
// 绝对 URL 拼装、沙箱产物取用、OpenAI size→宽高比换算。

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	sentinel "sentinel-go/sentinel"
)

// guessFileName 根据 MIME 类型猜测一个合适的文件名
func guessFileName(mime string) string {
	extMap := map[string]string{
		"image/jpeg":         "upload.jpg",
		"image/png":          "upload.png",
		"image/gif":          "upload.gif",
		"image/webp":         "upload.webp",
		"application/pdf":    "document.pdf",
		"application/msword": "document.doc",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "document.docx",
		"application/vnd.ms-excel": "spreadsheet.xls",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         "spreadsheet.xlsx",
		"application/vnd.ms-powerpoint":                                             "presentation.ppt",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation": "presentation.pptx",
		"text/plain":       "document.txt",
		"text/csv":         "data.csv",
		"application/json": "data.json",
		"text/markdown":    "document.md",
	}
	if name, ok := extMap[mime]; ok {
		return name
	}
	return "file"
}

// downloadURL 下载 HTTP/HTTPS URL 的内容，返回字节数据、文件名与 Content-Type（作 mimeHint）
func downloadURL(rawURL string) ([]byte, string, string, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return nil, "", "", fmt.Errorf("download %s: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("download %s: HTTP %d", rawURL, resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", fmt.Errorf("read body %s: %w", rawURL, err)
	}

	// 从 Content-Type 推断 MIME 与文件名
	contentType := resp.Header.Get("Content-Type")
	mimeType := strings.Split(contentType, ";")[0]
	mimeType = strings.TrimSpace(mimeType)
	fileName := guessFileName(mimeType)

	// 如果 URL 末尾有文件名，也可以用它
	if fileName == "file" {
		if idx := strings.LastIndex(rawURL, "/"); idx >= 0 {
			candidate := rawURL[idx+1:]
			// 去掉 query string
			if qIdx := strings.Index(candidate, "?"); qIdx >= 0 {
				candidate = candidate[:qIdx]
			}
			if strings.Contains(candidate, ".") {
				fileName = candidate
			}
		}
	}
	return data, fileName, mimeType, nil
}

// buildAbsoluteURL 将相对路径转换为绝对 URL
// 优先使用 cfg.BaseURL，其次从请求头推断
func buildAbsoluteURL(c *gin.Context, cfg *ServerConfig, path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if cfg.BaseURL != "" {
		base := strings.TrimRight(cfg.BaseURL, "/")
		return base + path
	}
	scheme := "http"
	if c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := c.Request.Host
	return scheme + "://" + host + path
}

// sandboxFilesForHandler 取本轮沙箱产物（优先 SandboxArtifacts，回退兼容旧 PDFArtifacts）。
func sandboxFilesForHandler(result *sentinel.ChatResult) []sentinel.SandboxArtifact {
	if len(result.SandboxArtifacts) > 0 {
		return result.SandboxArtifacts
	}
	out := make([]sentinel.SandboxArtifact, len(result.PDFArtifacts))
	for i, p := range result.PDFArtifacts {
		out[i] = sentinel.SandboxArtifact(p)
	}
	return out
}

// sizeToAspect 将 OpenAI 风格的 size 字符串转换为 ImageAspectRatio。
// 支持 "1:1" / "3:4" / "9:16" / "4:3" / "16:9" 宽高比直写，
// 以及兼容 OpenAI 像素格式 "256x256" / "1024x1024" / "1792x1024" / "1024x1792"。
func sizeToAspect(size string) sentinel.ImageAspectRatio {
	switch strings.TrimSpace(strings.ToLower(size)) {
	case "1:1", "256x256", "512x512", "1024x1024":
		return sentinel.ImageAspectSquare
	case "3:4":
		return sentinel.ImageAspectPortrait
	case "9:16", "1024x1792":
		return sentinel.ImageAspectStory
	case "4:3":
		return sentinel.ImageAspectLandscape
	case "16:9", "1792x1024":
		return sentinel.ImageAspectWidescreen
	default:
		return sentinel.ImageAspectAuto
	}
}
