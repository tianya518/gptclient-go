package sentinel

// thoughts.go —— thinking 模型的"思考步骤"解析：
//   - fetchTextdocs：主动拉取 textdocs 接口补全思考详情；
//   - extractThoughts：从 SSE 的 content_type="thoughts" 增量里抽取已完成步骤并去重推送。

import (
	"encoding/json"
	"fmt"
)

// fetchTextdocs 调用 textdocs API 获取思考步骤的详细内容
// textdocs 返回一个对象数组，每个对象包含 type、thought（含 summary/content）等字段
func (c *Client) fetchTextdocs(conversationID string) ([]ThinkStep, error) {
	apiPath := "/backend-api/conversation/" + conversationID + "/textdocs"
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/conversation/{conversation_id}/textdocs",
		}).
		Get(apiPath)
	if err != nil {
		return nil, fmt.Errorf("textdocs 请求失败: %w", err)
	}
	if resp.IsErrorState() {
		return nil, fmt.Errorf("textdocs 返回错误: status=%d body=%s", resp.StatusCode, resp.String()[:min(200, len(resp.String()))])
	}

	// textdocs 返回格式：{"textdocs": [{"type": 0, "thought": {"summary": "...", "content": "...", ...}}, ...]}
	// 或直接是数组
	rawBody := resp.String()
	c.logf("[textdocs] 原始响应 status=%d len=%d snippet=%s", resp.StatusCode, len(rawBody), rawBody[:min(500, len(rawBody))])

	var rawData interface{}
	if err := json.Unmarshal(resp.Bytes(), &rawData); err != nil {
		return nil, fmt.Errorf("textdocs 解析失败: %w", err)
	}

	var chunks []interface{}
	switch v := rawData.(type) {
	case map[string]interface{}:
		// 可能是 {"textdocs": [...]} 或 {"chunks": [...]}
		for _, key := range []string{"textdocs", "chunks", "items", "data"} {
			if arr, ok := v[key].([]interface{}); ok {
				chunks = arr
				break
			}
		}
		if chunks == nil {
			c.logf("[textdocs] 未知顶层结构, keys=%v", mapKeys(v))
		}
	case []interface{}:
		chunks = v
	}

	var steps []ThinkStep
	for _, chunkRaw := range chunks {
		chunk, ok := chunkRaw.(map[string]interface{})
		if !ok {
			continue
		}
		// type=0 是思考段落
		chunkType, _ := chunk["type"].(float64)
		if int(chunkType) != 0 {
			continue
		}
		thought, ok := chunk["thought"].(map[string]interface{})
		if !ok {
			continue
		}
		summary, _ := thought["summary"].(string)
		content, _ := thought["content"].(string)
		if summary == "" && content == "" {
			continue
		}
		steps = append(steps, ThinkStep{
			Summary: summary,
			Content: content,
		})
	}
	return steps, nil
}

// extractThoughts 从 content_type="thoughts" 消息的 thoughts 数组中提取已完成的思考步骤。
// SSE 流中的数组元素格式：{"summary": "...", "content": "...", "chunks": [...], "finished": true}
// 每个 finished=true 的步骤通过 \x00THINK_STEP\x00 标记推送一次（summary\x1Fcontent），去重处理。
func (c *Client) extractThoughts(thoughts []interface{}, result *ChatResult, handler StreamHandler) {
	if result.seenThoughtKeys == nil {
		result.seenThoughtKeys = make(map[string]bool)
	}
	for _, tRaw := range thoughts {
		t, ok := tRaw.(map[string]interface{})
		if !ok {
			continue
		}
		// SSE 格式：直接包含 summary, content, finished
		finished, _ := t["finished"].(bool)
		if !finished {
			continue
		}
		summary, _ := t["summary"].(string)
		content, _ := t["content"].(string)
		if summary == "" {
			continue
		}
		// 去重：同一个 summary 只推送一次
		if result.seenThoughtKeys[summary] {
			continue
		}
		result.seenThoughtKeys[summary] = true
		result.ThinkSteps = append(result.ThinkSteps, ThinkStep{Summary: summary, Content: content})
		c.logf("[thoughts] 新思考步骤: %s", summary)
		if handler != nil {
			payload := summary
			if content != "" {
				payload += "\x1F" + content
			}
			handler("\x00THINK_STEP\x00" + payload)
		}
	}
}
