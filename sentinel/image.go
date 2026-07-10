package sentinel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/imroc/req/v3"
)

// PollForImageFileID 已弃用：请使用 SSE 流中的 image_asset_pointer / ApplyArtifactsFromSignals。
// 保留仅为兼容旧调用方。
func (c *Client) PollForImageFileID(conversationID string) (string, error) {
	_ = conversationID
	return "", fmt.Errorf("PollForImageFileID 已弃用，请从 SSE 信号获取图片 file_id")
}

// ProxyImageByFileID 获取文件直链并代理直接将流输出到 http.ResponseWriter
func (c *Client) ProxyImageByFileID(fileID, conversationID string, w interface{}, reqUserAgent string) error {
	writer, ok := w.(http.ResponseWriter)
	if !ok {
		return fmt.Errorf("invalid http.ResponseWriter")
	}

	apiPath := fmt.Sprintf("/backend-api/files/download/%s?conversation_id=%s&inline=false", fileID, conversationID)
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "*/*",
			"Content-Type":          "application/json",
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/files/download/{fileId}",
		}).
		Get(apiPath)
	if err != nil || resp.StatusCode != 200 {
		return fmt.Errorf("download info failed: status=%d", resp.StatusCode)
	}

	var dr struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(resp.Bytes(), &dr); err != nil || dr.DownloadURL == "" {
		return fmt.Errorf("no download_url in response")
	}

	c.logf("[image] 提取到图片直链: %s", dr.DownloadURL)

	var imgResp *req.Response
	var errFetch error

	// 如果 DownloadURL 依然是 chatgpt.com 的内部地址（如 estuary/content），则必须携带原有的鉴权 Header（Bearer Token）
	// 如果是外部 CDN 直链（如 files.oaiusercontent.com），则使用干净的客户端防止双重鉴权或跨域被拦截
	isInternalURL := strings.Contains(dr.DownloadURL, "chatgpt.com")
	
	reqHeader := map[string]string{
		"User-Agent": reqUserAgent,
		"Accept":     "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8",
	}

	if isInternalURL {
		c.logf("[image] 内部链接，使用原生客户端进行代理")
		imgResp, errFetch = c.httpClient.R().SetHeaders(reqHeader).Get(dr.DownloadURL)
	} else {
		c.logf("[image] 外部 CDN 链接，使用干净客户端进行代理")
		cleanClient := req.C().ImpersonateChrome()
		imgResp, errFetch = cleanClient.R().SetHeaders(reqHeader).Get(dr.DownloadURL)
	}

	if errFetch != nil {
		return fmt.Errorf("proxy fetch image failed: %w", errFetch)
	}
	
	if imgResp.IsErrorState() {
		return fmt.Errorf("proxy fetch image returned error status: %d", imgResp.StatusCode)
	}
	
	imgData := imgResp.Bytes()
	contentType := imgResp.Header.Get("Content-Type")

	if contentType != "" {
		writer.Header()["Content-Type"] = []string{contentType}
	}
	writer.Header()["Cache-Control"] = []string{"public, max-age=31536000"} // 让浏览器永久缓存
	
	_, err = writer.Write(imgData)
	if err != nil {
		return fmt.Errorf("proxy write image failed: %w", err)
	}
	
	c.logf("[image] 代理传输完毕, %d bytes", len(imgData))
	return nil
}

// resolveFileDownloadURL 获取 files/download 直链。
func (c *Client) resolveFileDownloadURL(fileID, conversationID string) (string, error) {
	apiPath := fmt.Sprintf("/backend-api/files/download/%s?conversation_id=%s&inline=false", fileID, conversationID)
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "*/*",
			"Content-Type":          "application/json",
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/files/download/{fileId}",
		}).
		Get(apiPath)
	if err != nil || resp.StatusCode != 200 {
		return "", fmt.Errorf("download info failed: status=%d", resp.StatusCode)
	}
	var dr struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(resp.Bytes(), &dr); err != nil || dr.DownloadURL == "" {
		return "", fmt.Errorf("no download_url in response")
	}
	return dr.DownloadURL, nil
}

func (c *Client) fetchURLBytes(downloadURL, reqUserAgent string) ([]byte, string, error) {
	isInternal := strings.Contains(downloadURL, "chatgpt.com")
	reqHeader := map[string]string{
		"User-Agent": reqUserAgent,
		"Accept":     "*/*",
	}
	var imgResp *req.Response
	var errFetch error
	if isInternal {
		imgResp, errFetch = c.httpClient.R().SetHeaders(reqHeader).Get(downloadURL)
	} else {
		imgResp, errFetch = req.C().ImpersonateChrome().R().SetHeaders(reqHeader).Get(downloadURL)
	}
	if errFetch != nil {
		return nil, "", errFetch
	}
	if imgResp.IsErrorState() {
		return nil, "", fmt.Errorf("fetch %d", imgResp.StatusCode)
	}
	return imgResp.Bytes(), imgResp.Header.Get("Content-Type"), nil
}

// DownloadFileByFileID 下载生图/附件 file_id 对应二进制（供 base64 流式下发）。
func (c *Client) DownloadFileByFileID(conversationID, fileID string) ([]byte, string, error) {
	dl, err := c.resolveFileDownloadURL(fileID, conversationID)
	if err != nil {
		return nil, "", err
	}
	ua := c.userAgent
	if ua == "" {
		ua = "Mozilla/5.0"
	}
	return c.fetchURLBytes(dl, ua)
}
