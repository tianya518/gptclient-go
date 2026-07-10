package sentinel

// chat_sse.go —— SSE 事件解析与下发：把 delta/full 两种编码的事件还原为
// 正文（final 通道）/ 思考（analysis 通道）/ 生图产物，并通过 handler 增量回调。
// 同时含从事件里抽取 turn topic、图片任务 id 等的小工具。

import "strings"

// parseStreamHandoff 从 stream_handoff 事件中提取 resume_sse_endpoint 的 topic_id
func parseStreamHandoff(evt map[string]interface{}) (bool, string) {
	options, ok := evt["options"].([]interface{})
	if !ok {
		return false, ""
	}
	for _, optRaw := range options {
		opt, ok := optRaw.(map[string]interface{})
		if !ok {
			continue
		}
		if typ, _ := opt["type"].(string); typ == "subscribe_ws_topic" {
			topicID, _ := opt["topic_id"].(string)
			return topicID != "", topicID
		}
	}
	return false, ""
}

// checkImageTaskID 从 SSE 事件中提取图片任务 ID（兼容旧版 image_gen_task_id 和新版 ghostrider）
func checkImageTaskID(evt map[string]interface{}, result *ChatResult) {
	extractFromMeta := func(meta map[string]interface{}) {
		if tid, ok := meta["image_gen_task_id"].(string); ok && tid != "" {
			result.ImageTaskID = tid
			return
		}
		if result.ImageTaskID == "" {
			if _, ok := meta["ghostrider"]; ok {
				result.ImageTaskID = "ghostrider"
			}
		}
	}

	if v, ok := evt["v"].(map[string]interface{}); ok {
		if msg, ok := v["message"].(map[string]interface{}); ok {
			if meta, ok := msg["metadata"].(map[string]interface{}); ok {
				extractFromMeta(meta)
			}
		}
	}
}

func (result *ChatResult) noteAssistantChannel(channel string) {
	if channel == "analysis" {
		result.sawAnalysisChannel = true
	}
	if channel != "" {
		result.deltaChannel = channel
	}
}

func (result *ChatResult) isAnalysisStream() bool {
	if result.deltaChannel == "analysis" {
		return true
	}
	return result.deltaChannel == "" && result.sawAnalysisChannel
}

func (c *Client) emitThinkingDelta(result *ChatResult, text string, handler StreamHandler) {
	if text == "" {
		return
	}
	result.ThinkingText += text
	if handler != nil {
		handler("\x00THINK\x00" + text)
	}
}

func (result *ChatResult) shouldSkipImageGenToolBodyDelta(text string) bool {
	if !result.ExpectGeneratedImages {
		return false
	}
	if result.deltaChannel == "commentary" {
		return true
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return false
	}
	// ImageGen 工具参数 JSON 碎片（勿下发给客户端）
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, ",") || strings.Contains(t, "referenced_image_ids") {
		return true
	}
	return false
}

func (c *Client) emitBodyDelta(result *ChatResult, lastText *string, text string, handler StreamHandler) {
	if text == "" || result.shouldSkipImageGenToolBodyDelta(text) {
		return
	}
	newText := *lastText + text
	if newText == *lastText {
		return
	}
	toEmit := newText[result.emittedBodyLen:]
	*lastText = newText
	if result.deltaChannel == "final" {
		result.assistantFinalText = newText
	}
	if len(toEmit) == 0 {
		return
	}
	result.emittedBodyLen = len(newText)
	if handler != nil {
		handler(toEmit)
	}
}

func (c *Client) emitBodyFull(result *ChatResult, lastText *string, text, channel string, handler StreamHandler) {
	if text == "" {
		return
	}
	if text == *lastText {
		return
	}
	if len(text) < len(*lastText) && strings.HasPrefix(*lastText, text) {
		return
	}
	if len(text) <= len(*lastText) {
		return
	}
	*lastText = text
	if channel == "final" {
		result.assistantFinalText = text
		c.logf("[reply-final] channel=final len=%d", len([]rune(text)))
	}
	toEmit := text[result.emittedBodyLen:]
	if len(toEmit) == 0 {
		return
	}
	result.emittedBodyLen = len(text)
	if handler != nil {
		handler(toEmit)
	}
}

// processDeltaSSE 处理 delta 编码模式的 SSE 事件
// ChatGPT delta 格式有多种变体：
//
//	A) 顶层 patch：{"p":"/message/content/parts/0","o":"append","v":"text"}
//	B) 简写 append：{"v":"text"}（省略 p/o，隐含对 parts/0 的追加）
//	C) 消息对象 add：{"p":"","o":"add","v":{"message":{...}}}
//	D) 完成 patch 数组：{"p":"","o":"patch","v":[...patches...]}
func (c *Client) processDeltaSSE(evt map[string]interface{}, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler) {
	pPath, _ := evt["p"].(string)
	pOp, _ := evt["o"].(string)

	// 格式 A：顶层 append patch
	if pOp == "append" {
		if result.ExpectGeneratedImages && (pPath == "/message/content/text" || strings.HasPrefix(pPath, "/message/content/text")) {
			return
		}
		if pPath == "/message/content/parts/0" {
			if text, ok := evt["v"].(string); ok && text != "" {
				if result.isAnalysisStream() {
					c.emitThinkingDelta(result, text, handler)
				} else {
					c.emitBodyDelta(result, lastText, text, handler)
				}
			}
			return
		}
	}

	v := evt["v"]

	// 格式 B：只有 v 字段，且是字符串 → 隐含 append
	_, hasP := evt["p"]
	_, hasO := evt["o"]
	if !hasP && !hasO {
		if text, ok := v.(string); ok && text != "" {
			if result.isAnalysisStream() {
				c.emitThinkingDelta(result, text, handler)
			} else {
				c.emitBodyDelta(result, lastText, text, handler)
			}
			return
		}
	}

	// 格式 C：v 是包含 message 的 map（消息对象初始化或 final channel）
	if vMap, ok := v.(map[string]interface{}); ok {
		if msgRaw, exists := vMap["message"]; exists {
			if msg, ok := msgRaw.(map[string]interface{}); ok {
				result.noteTurnExchangeFromMessage(msg)
				author := getNestedString(msg, "author", "role")
				channel, _ := msg["channel"].(string)
				msgID, _ := msg["id"].(string)

				if author == "assistant" && msgID != "" {
					result.LastAssistantMsgID = msgID
					result.noteAssistantChannel(channel)

					// content_type="thoughts"：解析思考步骤（summary + content）
					if content, ok := msg["content"].(map[string]interface{}); ok {
						if ct, _ := content["content_type"].(string); ct == "thoughts" {
							if thoughts, ok := content["thoughts"].([]interface{}); ok {
								c.extractThoughts(thoughts, result, handler)
							}
						}
					}
					if result.ExpectGeneratedImages {
						c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "ws_turn_delta")
					}
				}
				if author == "tool" {
					if name := getNestedString(msg, "author", "name"); name != "" {
						lower := strings.ToLower(name)
						if strings.Contains(lower, "dalle") || strings.Contains(lower, "image_gen") {
							result.sawImageGenTool = true
						}
					}
					if result.ExpectGeneratedImages {
						c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "sse_tool_add")
					}
					if meta, ok := msg["metadata"].(map[string]interface{}); ok {
						if tid, ok := meta["image_gen_task_id"].(string); ok && tid != "" {
							result.ImageTaskID = tid
						}
						// 新版 ghostrider 异步生图：没有 image_gen_task_id，用 "ghostrider" 作为触发标志
						if result.ImageTaskID == "" {
							if _, ok := meta["ghostrider"]; ok {
								result.ImageTaskID = "ghostrider"
							}
						}
						// 思考模型：reasoning_title 是每步工具调用的思考标题
						if title, ok := meta["reasoning_title"].(string); ok && title != "" {
							// 同时取 content.parts[0] 作为执行输出
							execOutput := ""
							if content, ok := msg["content"].(map[string]interface{}); ok {
								if text, ok := content["text"].(string); ok {
									execOutput = text
								} else if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
									if s, ok := parts[0].(string); ok {
										execOutput = s
									}
								}
							}
							if handler != nil {
								payload := title
								if execOutput != "" {
									payload += "\x1F" + execOutput // \x1F 单元分隔符
								}
								handler("\x00THINK_STEP\x00" + payload)
							}
						}
					}
				}
				// final channel 上的完整文本（网页端可见的最终 JSON/正文）
				if author == "assistant" && channel == "final" {
					if text := getFirstStringPart(msg); text != "" {
						result.noteAssistantChannel("final")
						c.emitBodyFull(result, lastText, text, "final", handler)
					}
				}
			}
		}
	}

	// 格式 D：v 是 patches 数组（批量 patch）
	if patches, ok := v.([]interface{}); ok {
		for _, p := range patches {
			if patch, ok := p.(map[string]interface{}); ok {
				pp, _ := patch["p"].(string)
				po, _ := patch["o"].(string)
				if result.ExpectGeneratedImages && po == "append" && (pp == "/message/content/text" || strings.HasPrefix(pp, "/message/content/text")) {
					continue
				}
				if pp == "/message/content/parts/0" && po == "append" {
					if text, ok := patch["v"].(string); ok && text != "" {
						if result.isAnalysisStream() {
							c.emitThinkingDelta(result, text, handler)
						} else {
							c.emitBodyDelta(result, lastText, text, handler)
						}
					}
					continue
				}
				if result.ExpectGeneratedImages && strings.HasPrefix(pp, "/message/content/parts/") &&
					(po == "append" || po == "add" || po == "replace") {
					if partMap, ok := patch["v"].(map[string]interface{}); ok {
						if partMap["content_type"] == "image_asset_pointer" {
							ap, _ := partMap["asset_pointer"].(string)
							if fileID := extractFileID(ap); fileID != "" {
								p := ParsedGeneratedImage{FileID: fileID}
								if meta, ok := partMap["metadata"].(map[string]interface{}); ok {
									if dalle, ok := meta["dalle"].(map[string]interface{}); ok {
										p.GenID, _ = dalle["gen_id"].(string)
									}
								}
								c.noteGeneratedImageRevision(result, opts, p, "ws_part_patch")
							}
							continue
						}
					}
				}
			}
		}
	}
}

// processFullSSE 处理非 delta 编码模式的 SSE 事件
func (c *Client) processFullSSE(evt map[string]interface{}, result *ChatResult, opts ChatOptions, lastText *string, handler StreamHandler) {
	msgRaw, exists := evt["message"]
	if !exists {
		return
	}
	msg, ok := msgRaw.(map[string]interface{})
	if !ok {
		return
	}

	author := getNestedString(msg, "author", "role")
	channel, _ := msg["channel"].(string)
	msgID, _ := msg["id"].(string)

	if author == "assistant" && msgID != "" {
		result.LastAssistantMsgID = msgID

		// content_type="thoughts"：解析思考步骤（summary + content）
		if content, ok := msg["content"].(map[string]interface{}); ok {
			if ct, _ := content["content_type"].(string); ct == "thoughts" {
				if thoughts, ok := content["thoughts"].([]interface{}); ok {
					c.extractThoughts(thoughts, result, handler)
				}
			}
		}
		if result.ExpectGeneratedImages {
			c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "ws_turn_full")
		}
	}

	if meta, ok := msg["metadata"].(map[string]interface{}); ok {
		if tid, ok := meta["image_gen_task_id"].(string); ok && tid != "" {
			result.ImageTaskID = tid
		}
		// 思考模型：tool 消息中的 reasoning_title 是每步思考标题
		if author == "tool" {
			if result.ExpectGeneratedImages {
				c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "sse_tool_full")
			}
			if title, ok := meta["reasoning_title"].(string); ok && title != "" {
				execOutput := ""
				if content, ok := msg["content"].(map[string]interface{}); ok {
					if text, ok := content["text"].(string); ok {
						execOutput = text
					} else if parts, ok := content["parts"].([]interface{}); ok && len(parts) > 0 {
						if s, ok := parts[0].(string); ok {
							execOutput = s
						}
					}
				}
				if handler != nil {
					payload := title
					if execOutput != "" {
						payload += "\x1F" + execOutput
					}
					handler("\x00THINK_STEP\x00" + payload)
				}
			}
		}
	}

	if author == "assistant" {
		text := getFirstStringPart(msg)
		if text == "" {
			return
		}
		result.noteAssistantChannel(channel)
		if channel == "analysis" {
			if len(text) > len(result.ThinkingText) {
				c.emitThinkingDelta(result, text[len(result.ThinkingText):], handler)
				result.ThinkingText = text
			}
		} else if channel == "final" {
			c.emitBodyFull(result, lastText, text, "final", handler)
		} else if !result.sawAnalysisChannel {
			c.emitBodyFull(result, lastText, text, channel, handler)
		}
	}
}
