package sentinel

import (
	"fmt"
	"strings"
)

// DeleteConversation 软删除 ChatGPT 会话（对齐官网侧栏「删除」）。
//
// 官网实测（2026-07-22 MCP 抓包）：
//
//	PATCH /backend-api/conversation/{conversation_id}
//	Body: {"is_visible": false}
//	Resp: {"success": true}
//
// 这是软删除（隐藏），不是 HTTP DELETE / 物理删除。
func (c *Client) DeleteConversation(conversationID string) error {
	id := strings.TrimSpace(conversationID)
	if id == "" {
		return fmt.Errorf("conversation_id is required")
	}

	apiPath := "/backend-api/conversation/" + id
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Content-Type":          "application/json",
			"Accept":                "*/*",
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/conversation/{conversation_id}",
		}).
		SetBody(map[string]interface{}{
			"is_visible": false,
		}).
		Patch(apiPath)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("delete conversation %d: %s", resp.StatusCode, truncateStr(resp.String(), 300))
	}

	c.logf("[conversation] deleted (is_visible=false) id=%s status=%d", id, resp.StatusCode)

	// 若删的是当前会话，清空本地上下文，避免后续请求继续挂在已隐藏的 conversation_id 上
	if c.conversationID == id {
		c.ResetSession()
	}
	return nil
}
