package server

// handler_chat_input.go —— 入站请求内容解析：从 OpenAI 风格 messages 里
// 提取最后一条 user 文本、system 提示词，以及多模态里的图片/文件（data URL / http URL）。

import "strings"

// parseMessageContent 解析多模态内容或纯文本内容
func parseMessageContent(c interface{}) (text string, images []string) {
	if c == nil {
		return
	}
	if s, ok := c.(string); ok {
		return s, nil
	}
	if arr, ok := c.([]interface{}); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				t, _ := m["type"].(string)
				if t == "text" {
					if txt, ok := m["text"].(string); ok {
						text += txt
					}
				} else if t == "image_url" {
					if imgUrl, ok := m["image_url"].(map[string]interface{}); ok {
						if url, ok := imgUrl["url"].(string); ok {
							images = append(images, url)
						}
					}
				} else if t == "file" {
					if filePart, ok := m["file"].(map[string]interface{}); ok {
						if fileData, ok := filePart["file_data"].(string); ok && fileData != "" {
							// data:application/pdf;base64,... 格式，直接复用 data URL 通道
							images = append(images, fileData)
						}
					}
				}
			}
		}
	}
	return
}

// extractUserMessage 从 messages 中提取最后一条 user 消息和 system 提示词
func extractUserMessage(messages []Message) (userMsg string, systemPrompt string, images []string) {
	// 找 system prompt
	for _, m := range messages {
		if strings.ToLower(m.Role) == "system" {
			systemPrompt, _ = parseMessageContent(m.Content)
			break
		}
	}
	// 找最后一条 user 消息
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.ToLower(messages[i].Role) == "user" {
			userMsg, images = parseMessageContent(messages[i].Content)
			break
		}
	}
	return
}
