package sentinel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// PollForImageFileID 轮询对话详情并提取 DALL-E 生成的图片 fileID
// 等待直到 `status` != "in_progress"
func (c *Client) PollForImageFileID(conversationID string) (string, error) {
	c.logf("[image] 等待图片生成...")
	timeout := 60 * time.Second
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)

		// 调用 /backend-api/conversation/{id}
		resp, err := c.httpClient.R().
			SetPathParam("id", conversationID).
			Get("/backend-api/conversation/{id}")
		if err != nil || resp.StatusCode != 200 {
			continue // 忽略错误重试
		}

		var data struct {
			Mapping map[string]struct {
				Message struct {
					ID     string `json:"id"`
					Status string `json:"status"`
					Author struct {
						Role string `json:"role"`
						Name string `json:"name"`
					} `json:"author"`
					Content struct {
						ContentType string        `json:"content_type"`
						Parts       []interface{} `json:"parts"`
					} `json:"content"`
					Metadata map[string]interface{} `json:"metadata"`
				} `json:"message"`
			} `json:"mapping"`
		}
		if err := json.Unmarshal(resp.Bytes(), &data); err != nil {
			continue
		}

		fileIDs := make(map[string]bool)
		for _, node := range data.Mapping {
			msg := node.Message
			if msg.Author.Role == "tool" && msg.Author.Name == "dalle.text2im" {
				if msg.Status == "in_progress" {
					continue // 还在生成中，进入下一次轮询
				}
				// 完成，提取 parts 里的 asset_pointer
				if msg.Content.ContentType == "multimodal_text" {
					for _, p := range msg.Content.Parts {
						if partMap, ok := p.(map[string]interface{}); ok {
							if ap, ok := partMap["asset_pointer"].(string); ok {
								if fid := extractFileID(ap); fid != "" {
									fileIDs[fid] = true
								}
							}
						}
					}
				}
			}

			if meta := msg.Metadata; meta != nil {
				if refs, ok := meta["content_references"].([]interface{}); ok {
					for _, ref := range refs {
						if refMap, ok := ref.(map[string]interface{}); ok {
							if ap, ok := refMap["asset_pointer"].(string); ok {
								if fid := extractFileID(ap); fid != "" {
									fileIDs[fid] = true
								}
							}
						}
					}
				}
			}
		}

		for fid := range fileIDs {
			// 直接返回 fileID
			return fid, nil
		}
	}

	c.logf("[image] 超时，未能获取图片 fileID")
	return "", fmt.Errorf("image poll timeout")
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
