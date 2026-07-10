package sentinel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// PDFArtifact 与 SandboxArtifact 同构（兼容旧字段名）。
type PDFArtifact = SandboxArtifact

func filterPDFArtifacts(arts []SandboxArtifact) []PDFArtifact {
	var out []PDFArtifact
	for _, a := range arts {
		if strings.HasSuffix(strings.ToLower(a.FileName), ".pdf") {
			out = append(out, PDFArtifact(a))
		}
	}
	return out
}

func sandboxNames(arts []SandboxArtifact) []string {
	names := make([]string, len(arts))
	for i, a := range arts {
		names[i] = a.FileName
	}
	return names
}

func pdfNames(pdfs []PDFArtifact) []string {
	return sandboxNames(artsFromPDF(pdfs))
}

func artsFromPDF(pdfs []PDFArtifact) []SandboxArtifact {
	out := make([]SandboxArtifact, len(pdfs))
	for i, p := range pdfs {
		out[i] = SandboxArtifact(p)
	}
	return out
}

// resolvePDFDownloadURL 调用 interpreter/download 获取沙箱文件下载直链（pdf/txt 等）。
//
// 2026-07 官网实测：该接口已由 POST 改为 GET（POST 返回 405 Method Not Allowed），
// message_id / sandbox_path 经 query 传递。返回的是真正的沙箱产物（如 PDF）直链；
// 注意勿用 execution_output 里的 file_id 走 files/download —— 那个 id 常指向 PDF 的
// PNG 预览图（_sandbox_pdf_render/page-1.png），并非原始文件。
func (c *Client) resolvePDFDownloadURL(conversationID, messageID, sandboxPath string) (string, error) {
	basePath := "/backend-api/conversation/" + conversationID + "/interpreter/download"
	apiPath := basePath + "?message_id=" + url.QueryEscape(messageID) +
		"&sandbox_path=" + url.QueryEscape(sandboxPath)

	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "application/json",
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/conversation/{conversation_id}/interpreter/download",
		}).
		Get(apiPath)
	if err != nil {
		return "", fmt.Errorf("interpreter/download request: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("interpreter/download %d: %s", resp.StatusCode, truncateStr(resp.String(), 200))
	}

	var out struct {
		DownloadURL string `json:"download_url"`
	}
	if err := json.Unmarshal(resp.Bytes(), &out); err != nil {
		return "", fmt.Errorf("parse download response: %w", err)
	}
	if out.DownloadURL == "" {
		return "", fmt.Errorf("empty download_url")
	}
	return out.DownloadURL, nil
}

// ProxyPDFBySandboxPath 代理下载沙箱文件并写入 ResponseWriter。
func (c *Client) ProxyPDFBySandboxPath(conversationID, messageID, sandboxPath string, w interface{}, reqUserAgent string) error {
	writer, ok := w.(http.ResponseWriter)
	if !ok {
		return fmt.Errorf("invalid ResponseWriter")
	}

	downloadURL, err := c.resolvePDFDownloadURL(conversationID, messageID, sandboxPath)
	if err != nil {
		return err
	}

	ua := reqUserAgent
	if ua == "" {
		ua = c.userAgent
	}
	// 直链常为 chatgpt.com 内部地址（estuary/content），必须走带代理+Bearer 的 httpClient；
	// 若为外部 CDN 则用干净的 Chrome 指纹客户端。fetchURLBytes 已按 host 自动区分。
	data, contentType, err := c.fetchURLBytes(downloadURL, ua)
	if err != nil {
		return fmt.Errorf("download file: %w", err)
	}

	filename := sandboxPath[strings.LastIndex(sandboxPath, "/")+1:]
	writer.Header().Set("Content-Disposition", "attachment; filename=\""+url.PathEscape(filename)+"\"")
	switch {
	case strings.HasSuffix(strings.ToLower(filename), ".pdf"):
		writer.Header().Set("Content-Type", "application/pdf")
	case contentType != "":
		writer.Header().Set("Content-Type", contentType)
	default:
		writer.Header().Set("Content-Type", "application/octet-stream")
	}
	_, err = writer.Write(data)
	return err
}

// DownloadSandboxFile 下载沙箱产物二进制（供 base64 流式下发）。
func (c *Client) DownloadSandboxFile(conversationID, messageID, sandboxPath string) ([]byte, string, error) {
	downloadURL, err := c.resolvePDFDownloadURL(conversationID, messageID, sandboxPath)
	if err != nil {
		return nil, "", err
	}
	ua := c.userAgent
	if ua == "" {
		ua = "Mozilla/5.0"
	}
	data, _, err := c.fetchURLBytes(downloadURL, ua)
	if err != nil {
		return nil, "", err
	}
	filename := sandboxPath[strings.LastIndex(sandboxPath, "/")+1:]
	return data, guessMimeFromName(filename), nil
}

// encodeSandboxPathForQuery URL 编码 sandbox_path。
func encodeSandboxPathForQuery(sandboxPath string) string {
	return url.QueryEscape(sandboxPath)
}
