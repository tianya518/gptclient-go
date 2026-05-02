package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/imroc/req/v3"
)

// UploadedFile 是三步上传后沉淀的"可 attach 给 messages"的元数据。
type UploadedFile struct {
	FileID      string `json:"file_id"`
	FileName    string `json:"file_name"`
	FileSize    int    `json:"file_size"`
	MimeType    string `json:"mime_type"`
	UseCase     string `json:"use_case"`          // 图片: multimodal, 文件: my_files
	Width       int    `json:"width,omitempty"`   // 仅图片
	Height      int    `json:"height,omitempty"`  // 仅图片
	DownloadURL string `json:"download_url"`      // POST /uploaded 返回,通常不直接用
}

// UploadFile 执行完整三步上传。
func (c *Client) UploadFile(ctx context.Context, data []byte, fileName string) (*UploadedFile, error) {
	if len(data) == 0 {
		return nil, errors.New("empty file data")
	}
	mime, ext := sniffMime(data)
	useCase := "multimodal"
	if !strings.HasPrefix(mime, "image/") {
		useCase = "my_files"
	}
	if fileName == "" {
		fileName = fmt.Sprintf("file-%d%s", len(data), ext)
	}

	out := &UploadedFile{
		FileName: fileName,
		FileSize: len(data),
		MimeType: mime,
		UseCase:  useCase,
	}
	if strings.HasPrefix(mime, "image/") {
		if img, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			out.Width = img.Width
			out.Height = img.Height
		}
	}

	// ---- Step 1: POST /backend-api/files ----
	step1Body := map[string]interface{}{
		"file_name": fileName,
		"file_size": len(data),
		"use_case":  useCase,
	}
	if out.Width > 0 && out.Height > 0 {
		step1Body["height"] = out.Height
		step1Body["width"] = out.Width
	}

	var step1Resp struct {
		FileID    string `json:"file_id"`
		UploadURL string `json:"upload_url"`
		Status    string `json:"status"`
	}

	resp1, err := c.httpClient.R().
		SetContext(ctx).
		SetBody(step1Body).
		Post("/backend-api/files")
	if err != nil {
		return nil, fmt.Errorf("step1 post files failed: %w", err)
	}
	if resp1.IsErrorState() {
		return nil, fmt.Errorf("step1 create file failed: %s", resp1.String())
	}
	if err := json.Unmarshal(resp1.Bytes(), &step1Resp); err != nil {
		return nil, fmt.Errorf("step1 decode failed: %w", err)
	}
	if step1Resp.FileID == "" || step1Resp.UploadURL == "" {
		return nil, fmt.Errorf("step1 empty response: %s", resp1.String())
	}
	out.FileID = step1Resp.FileID

	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// ---- Step 2: PUT upload_url (Azure Blob) ----
	// 我们必须使用一个全新的不带 OpenAI auth header 的 Client 发送请求给 Azure
	azureClient := req.C().ImpersonateChrome()
	resp2, err := azureClient.R().
		SetContext(ctx).
		SetHeader("Content-Type", mime).
		SetHeader("x-ms-blob-type", "BlockBlob").
		SetHeader("x-ms-version", "2020-04-08").
		SetHeader("Origin", "https://chatgpt.com").
		SetHeader("User-Agent", c.userAgent).
		SetHeader("Accept", "application/json, text/plain, */*").
		SetHeader("Accept-Language", "en-US,en;q=0.8").
		SetHeader("Referer", "https://chatgpt.com/").
		SetBody(data).
		Put(step1Resp.UploadURL)
	if err != nil {
		return nil, fmt.Errorf("step2 azure upload failed: %w", err)
	}
	if resp2.IsErrorState() {
		return nil, fmt.Errorf("step2 azure upload error: %s", resp2.String())
	}

	// ---- Step 3: POST /backend-api/files/{file_id}/uploaded ----
	var step3Resp struct {
		Status      string `json:"status"`
		DownloadURL string `json:"download_url"`
	}
	resp3, err := c.httpClient.R().
		SetContext(ctx).
		SetBody(map[string]interface{}{}).
		Post("/backend-api/files/" + step1Resp.FileID + "/uploaded")
	if err != nil {
		return nil, fmt.Errorf("step3 register uploaded failed: %w", err)
	}
	if resp3.IsErrorState() {
		return nil, fmt.Errorf("step3 register uploaded error: %s", resp3.String())
	}
	_ = json.Unmarshal(resp3.Bytes(), &step3Resp)
	out.DownloadURL = step3Resp.DownloadURL

	return out, nil
}

func isTransientNetErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	s := err.Error()
	for _, kw := range []string{
		"EOF",
		"connection reset",
		"connection refused",
		"broken pipe",
		"no route to host",
		"network is unreachable",
		"TLS handshake",
		"tls: handshake",
		"utls handshake",
		"i/o timeout",
		"unexpected EOF",
		"server closed connection",
		"use of closed network connection",
	} {
		if strings.Contains(s, kw) {
			return true
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}

// Attachment 是 messages[*].metadata.attachments[*] 的序列化对象。
type Attachment struct {
	ID       string `json:"id"`
	MimeType string `json:"mimeType"`
	Name     string `json:"name"`
	Size     int    `json:"size"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// ToAttachment 把一个已上传的 file 转成 messages.metadata.attachments 里的条目。
func (u *UploadedFile) ToAttachment() Attachment {
	a := Attachment{ID: u.FileID, MimeType: u.MimeType, Name: u.FileName, Size: u.FileSize}
	if u.UseCase == "multimodal" {
		a.Width = u.Width
		a.Height = u.Height
	}
	return a
}

// AssetPointerPart 是 messages[*].content.parts 里的一项(图片),
// 用于把 file-service:// 挂到多模态消息最前面。
type AssetPointerPart struct {
	ContentType  string `json:"content_type,omitempty"` // "image_asset_pointer"
	AssetPointer string `json:"asset_pointer"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	SizeBytes    int    `json:"size_bytes,omitempty"`
}

// ToAssetPointerPart 返回 multimodal_text.parts 里 insert 在 prompt 前的那一项。
func (u *UploadedFile) ToAssetPointerPart() AssetPointerPart {
	return AssetPointerPart{
		ContentType:  "image_asset_pointer",
		AssetPointer: "file-service://" + u.FileID,
		Width:        u.Width,
		Height:       u.Height,
		SizeBytes:    u.FileSize,
	}
}

func sniffMime(data []byte) (mime, ext string) {
	n := 512
	if len(data) < n {
		n = len(data)
	}
	mime = http.DetectContentType(data[:n])
	if i := strings.Index(mime, ";"); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	switch {
	case strings.EqualFold(mime, "image/jpeg"):
		ext = ".jpg"
	case strings.EqualFold(mime, "image/png"):
		ext = ".png"
	case strings.EqualFold(mime, "image/gif"):
		ext = ".gif"
	case strings.EqualFold(mime, "image/webp"):
		ext = ".webp"
	case strings.EqualFold(mime, "application/pdf"):
		ext = ".pdf"
	default:
		ext = ""
	}
	return
}
