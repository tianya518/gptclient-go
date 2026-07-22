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
	_ "golang.org/x/image/webp"
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
// mimeHint 可选：来自 data URL header 或 HTTP Content-Type；为空或不可靠时根据文件内容嗅探。
func (c *Client) UploadFile(ctx context.Context, data []byte, fileName, mimeHint string) (*UploadedFile, error) {
	if len(data) == 0 {
		return nil, errors.New("empty file data")
	}
	mime, ext := resolveMime(data, mimeHint)
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
		// 官网 ImageAssetPointer 强制要求 width/height；解码失败时给保守默认值
		if out.Width <= 0 || out.Height <= 0 {
			out.Width, out.Height = 1024, 1024
		}
	}

	// ---- Step 1: POST /backend-api/files ----
	// 字段以官网实测为准：file_name/file_size/use_case/mime_type 必备，图片再带 width/height。
	step1Body := map[string]interface{}{
		"file_name": fileName,
		"file_size": len(data),
		"use_case":  useCase,
		"mime_type": mime,
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
//
// 字段命名以 2026-07 官网实测为准（图生图/文件上传抓包）：
//   - mime_type 为蛇形（旧版曾用驼峰 mimeType，现已统一蛇形）；
//   - source 标注来源，本地上传固定 "local"；
//   - width/height 仅图片附带。
type Attachment struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int    `json:"size"`
	MimeType string `json:"mime_type"`
	Source   string `json:"source,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// ToAttachment 把一个已上传的 file 转成 messages.metadata.attachments 里的条目。
func (u *UploadedFile) ToAttachment() Attachment {
	a := Attachment{ID: u.FileID, MimeType: u.MimeType, Name: u.FileName, Size: u.FileSize, Source: "local"}
	if u.UseCase == "multimodal" {
		a.Width = u.Width
		a.Height = u.Height
	}
	return a
}

// AssetPointerPart 是 messages[*].content.parts 里的一项(图片),
// 用于把参考图挂到多模态消息最前面。
type AssetPointerPart struct {
	ContentType  string `json:"content_type,omitempty"` // "image_asset_pointer"
	AssetPointer string `json:"asset_pointer"`
	// width/height 不能 omitempty：官网 pydantic 校验缺失即 422
	Width     int `json:"width"`
	Height    int `json:"height"`
	SizeBytes int `json:"size_bytes,omitempty"`
}

// ToAssetPointerPart 返回 multimodal_text.parts 里 insert 在 prompt 前的那一项。
//
// asset_pointer 前缀：2026-07 官网实测上传参考图使用 sediment://（旧版为 file-service://）。
// 与生图产出的 asset_pointer 前缀保持一致，避免协议漂移。
func (u *UploadedFile) ToAssetPointerPart() AssetPointerPart {
	return AssetPointerPart{
		ContentType:  "image_asset_pointer",
		AssetPointer: "sediment://" + u.FileID,
		Width:        u.Width,
		Height:       u.Height,
		SizeBytes:    u.FileSize,
	}
}

// resolveMime 优先嗅探真实内容；hint 与魔数冲突时（如速卖通 webp 标成 jpeg）以嗅探为准。
func resolveMime(data []byte, mimeHint string) (mime, ext string) {
	sniffed, sniffExt := sniffMime(data)
	if isWebP(data) {
		sniffed, sniffExt = "image/webp", ".webp"
	}
	hint := normalizeMime(mimeHint)
	if sniffed != "" && sniffed != "application/octet-stream" &&
		hint != "" && hint != sniffed &&
		strings.HasPrefix(sniffed, "image/") {
		return sniffed, sniffExt
	}
	if hint != "" && hint != "application/octet-stream" {
		ext = extFromMime(hint)
		if ext == "" {
			ext = sniffExt
		}
		return hint, ext
	}
	return sniffed, sniffExt
}

func isWebP(data []byte) bool {
	// RIFF....WEBP
	return len(data) >= 12 &&
		string(data[0:4]) == "RIFF" &&
		string(data[8:12]) == "WEBP"
}

func normalizeMime(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, ";"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

func extFromMime(mime string) string {
	switch strings.ToLower(normalizeMime(mime)) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "application/msword":
		return ".doc"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return ".docx"
	case "application/vnd.ms-excel":
		return ".xls"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return ".xlsx"
	case "application/vnd.ms-powerpoint":
		return ".ppt"
	case "application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return ".pptx"
	case "text/plain":
		return ".txt"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	case "text/markdown":
		return ".md"
	default:
		return ""
	}
}

func sniffMime(data []byte) (mime, ext string) {
	n := 512
	if len(data) < n {
		n = len(data)
	}
	mime = http.DetectContentType(data[:n])
	ext = extFromMime(mime)
	return mime, ext
}
