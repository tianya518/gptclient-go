package sentinel

// chat.go —— 对话入口与请求编排。
//
// 一轮对话的骨架：Chat/ChatStream 组装请求体（含图片/文档附件、picture_v2、thinking_effort），
// 预建 WebSocket，然后交给 streamConversation 处理 SSE 与后续分发。各细分职责拆到同包其它文件：
//   - chat_stream.go：主 SSE 读取循环与 SSE 结束后的分发（生图轮询 / 定稿 / WS catchup）
//   - chat_sse.go   ：SSE 事件解析与正文/思考增量下发
//   - chat_ws.go    ：WebSocket 拨号、topic 订阅循环与帧处理
//   - image_handoff.go / image_ws.go / image_revision.go：生图路由探测与收图
//   - thoughts.go   ：thinking 步骤解析

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ImageAspectRatio 图片宽高比
type ImageAspectRatio string

const (
	ImageAspectAuto       ImageAspectRatio = ""     // 自动（默认）
	ImageAspectSquare     ImageAspectRatio = "1:1"  // 方形
	ImageAspectPortrait   ImageAspectRatio = "3:4"  // 竖版
	ImageAspectStory      ImageAspectRatio = "9:16" // 故事版
	ImageAspectLandscape  ImageAspectRatio = "4:3"  // 横版
	ImageAspectWidescreen ImageAspectRatio = "16:9" // 宽屏
)

// ChatOptions 对话请求参数
type ChatOptions struct {
	Text           string
	Images         []UploadedFile
	ForcePictureV2 bool
	// ThinkingEffort 覆盖思考深度："standard" | "extended"（留空则按模型自动：thinking 模型用 extended）。
	// extended 触发深度推理，是官网"多图/多风格分别生成"的关键（普通 standard 常合成一张）。
	ThinkingEffort string
	// ImageAspect 仅在 ForcePictureV2=true 时生效，指定生成图片的宽高比
	ImageAspect ImageAspectRatio
	// Artifacts 产物（生图/沙箱文件）流式侧信道；正文仍走 StreamHandler
	Artifacts ArtifactStreamConfig
	// OnConversationID 首次得知 conversation_id 时回调（server 用于提前 Register，以便流中即可拉取 /api/image/proxy）
	OnConversationID func(convID string)
}

// resolveThinkingEffort 决定请求体 thinking_effort 的值。
// 返回值：
//   - "extended" / "standard"：写入请求体（thinking 模型）
//   - ""：不写入 thinking_effort 字段（极速/gpt-5-3-instant/o3 等官网原生不携带此字段）
//
// 优先级：opts.ThinkingEffort 显式设置 > 按当前 model 名称推断。
// 推断规则（依据 2026-07-09 官网 MCP 抓包）：
//   - 含 "thinking" 的 model（gpt-5-5-thinking / gpt-5-4-thinking）→ "extended"（高级默认）
//   - 其余（gpt-5-5 极速 / gpt-5-3-instant / o3）→ ""（不携带字段）
func (c *Client) resolveThinkingEffort(opts ChatOptions) string {
	if e := strings.TrimSpace(opts.ThinkingEffort); e != "" && e != "none" {
		return e
	}
	if strings.Contains(strings.ToLower(c.model), "thinking") {
		return "extended"
	}
	// 非 thinking model：不携带 thinking_effort（对齐官网行为）
	return ""
}

// Chat 发送一轮对话，返回完整结果（非流式）
func (c *Client) Chat(opts ChatOptions) (*ChatResult, error) {
	return c.ChatStream(opts, nil)
}

// ChatStream 发送一轮对话，通过 handler 回调实时接收增量文本
func (c *Client) ChatStream(opts ChatOptions, handler StreamHandler) (*ChatResult, error) {
	turnTraceID := GenerateUUID()

	c.logf("[step 1] 获取 conduit token...")
	conduitToken, err := c.getConduitToken(c.model, turnTraceID, runeSlice(opts.Text, 5))
	if err != nil {
		return nil, fmt.Errorf("get conduit token: %w", err)
	}

	c.logf("[step 2] 获取 sentinel token...")
	sentinelToken, proofToken, err := c.getSentinelToken()
	if err != nil {
		return nil, fmt.Errorf("get sentinel token: %w", err)
	}

	c.logf("[step 2.5] 建立 WebSocket 连接...")
	wsConn, err := c.dialChatWS()
	if err != nil {
		return nil, fmt.Errorf("dial ws: %w", err)
	}
	defer wsConn.Close()

	promptText := opts.Text
	if opts.ForcePictureV2 && opts.ImageAspect != ImageAspectAuto {
		promptText += "\n\n将宽高比设为 " + string(opts.ImageAspect)
	}

	// 区分图片（multimodal）和文档（my_files）
	// 图片需要插入 content.parts 作为 image_asset_pointer；文档只放 metadata.attachments
	var parts []interface{}
	hasImages := false
	for _, f := range opts.Images {
		if f.UseCase == "multimodal" {
			parts = append(parts, f.ToAssetPointerPart())
			hasImages = true
		}
	}
	parts = append(parts, promptText)

	contentType := "text"
	if hasImages {
		contentType = "multimodal_text"
	}

	attachments := []Attachment{}
	for _, f := range opts.Images {
		attachments = append(attachments, f.ToAttachment())
	}

	msgID := GenerateUUID()
	userMsgObj := map[string]interface{}{
		"id":          msgID,
		"author":      map[string]string{"role": "user"},
		"create_time": float64(time.Now().UnixMilli()) / 1000.0,
		"content": map[string]interface{}{
			"content_type": contentType,
			"parts":        parts,
		},
		"metadata": map[string]interface{}{
			"developer_mode_connector_ids": []string{},
			"selected_sources":             []string{},
			"selected_github_repos":        []string{},
			"selected_all_github_repos":    false,
			"serialization_metadata":       map[string]interface{}{"custom_symbol_offsets": []interface{}{}},
		},
	}
	if len(attachments) > 0 {
		userMsgObj["metadata"].(map[string]interface{})["attachments"] = attachments
	}

	systemHints := []string{}
	if opts.ForcePictureV2 {
		systemHints = append(systemHints, "picture_v2")
		meta := userMsgObj["metadata"].(map[string]interface{})
		meta["system_hints"] = systemHints
		// picture_v2 不能带 selected_sources，否则直接失败 (静默失败)
		delete(meta, "selected_sources")
	}

	body := map[string]interface{}{
		"action": "next",
		"messages": []map[string]interface{}{
			userMsgObj,
		},
		"parent_message_id":        c.parentMessageID,
		"model":                    c.model,
		"timezone_offset_min":      -480,
		"timezone":                 "Asia/Shanghai",
		"conversation_mode":        map[string]string{"kind": "primary_assistant"},
		"enable_message_followups": true,
		"system_hints":             systemHints,
		"supports_buffering":       true,
		"supported_encodings":      []string{"v1"},
		"client_contextual_info": map[string]interface{}{
			"is_dark_mode":      false,
			"time_since_loaded": int(math.Round(perfNowMs(c.startTime) / 1000.0)),
			"page_height":       1014,
			"page_width":        1055,
			"pixel_ratio":       1,
			"screen_height":     1080,
			"screen_width":      1920,
			"app_name":          "chatgpt.com",
		},
		"history_and_training_disabled":        c.tempMode,
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
		"client_prepare_state":                 "none",
	}
	// thinking_effort 仅在非空时写入请求体。
	// 极速(gpt-5-5) / gpt-5-3-instant / o3 官网不携带此字段，空值表示不发送。
	if te := c.resolveThinkingEffort(opts); te != "" {
		body["thinking_effort"] = te
	}
	if c.conversationID != "" {
		body["conversation_id"] = c.conversationID
	}

	convDesc := c.conversationID
	if convDesc == "" {
		convDesc = "(新对话)"
	}
	c.logf("[step 3] 发送对话: model=%s, conversation=%s, turn=%d", c.model, convDesc, c.turnCount+1)

	result, err := c.streamConversation(body, opts, sentinelToken, proofToken, conduitToken, turnTraceID, wsConn, handler)
	if err != nil {
		return nil, err
	}

	if result.ConversationID != "" {
		c.conversationID = result.ConversationID
	}
	if result.LastAssistantMsgID != "" {
		c.parentMessageID = result.LastAssistantMsgID
	}
	c.turnCount++

	c.logf("[info] conversation_id=%s, turn=%d, reply=%d字, thinking=%d字, final=%d字",
		c.conversationID, c.turnCount,
		len([]rune(result.Text)),
		len([]rune(result.ThinkingText)),
		len([]rune(result.assistantFinalText)))
	LogContentPreview(c.logf, "reply-text", result.Text)
	if result.ThinkingText != "" && result.ThinkingText != result.Text {
		LogContentPreview(c.logf, "reply-thinking", result.ThinkingText)
	}

	c.MergeApplyAndEmitArtifacts(result, opts)
	if len(result.SandboxArtifacts) > 0 {
		c.logf("[artifact] SSE 沙箱产物: %v", sandboxNames(result.SandboxArtifacts))
		result.Text = ""
	}
	if result.ExpectGeneratedImages && len(result.ImageFileIDs) > 0 {
		c.logf("[artifact] 生图 file_id: %v", result.ImageFileIDs)
	}

	return result, nil
}
