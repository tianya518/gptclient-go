package sentinel

// image_handoff.go —— 生图路由决策与收图交接。
//
// thinking 模型的主 SSE 极短且不含生图关键词，生图与"带附件的文本轮次"都走 async
// （SSE [DONE] 后经 GET /conversation 下发）。本文件负责：
//   - resolveImageHandoff：判定本轮是否走生图轮询路径；
//   - probeImageTurnViaConversation：SSE 无关键词时探测会话，区分生图 / 文本轮次；
//   - collectImagesViaPolling：关闭 WS 并转 GET /conversation 轮询收齐所有图片；
//   - extractFinalAssistantText / noteSandboxArtifactsFromText：文本轮次就地取回最终正文与沙箱产物。

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// resolveImageHandoff 判定本轮是否需要走"生图轮询"路径（并处理 thinking 场景 B 的探测副作用）。
//
// 触发条件（任一成立即为生图）：
//   - 显式 picture_v2（opts.ForcePictureV2）；
//   - SSE 阶段已识别出生图工具（sawImageGenTool）或拿到 image_gen 异步任务（ImageTaskID）；
//   - 场景 B（thinking 批量生图）：主 SSE 极短且不含生图关键词，但发生了 stream_handoff 且正文为空。
//     该路径图片全部经 GET /conversation 轮询下发（async_status 3/5→4），SSE 阶段无从判定，
//     故此处对 thinking 轮次探测一次 conversation（带重试）确认后再转轮询，避免漏图；
//     探测一旦发现 assistant 正文即判为文本，不影响普通文本流式。
//
// 返回 true 时，调用方应关闭预建 WS 并改用 collectImagesViaPolling 收图。
func (c *Client) resolveImageHandoff(result *ChatResult, opts ChatOptions, handoffTopicID string) bool {
	if c.DisableAutoImage || result == nil {
		return false
	}
	if result.ExpectGeneratedImages &&
		(opts.ForcePictureV2 || result.ImageTaskID != "" || result.sawImageGenTool) {
		return true
	}
	if result.ConversationID != "" && handoffTopicID != "" && result.assistantFinalText == "" &&
		strings.Contains(strings.ToLower(c.model), "thinking") {
		if c.probeImageTurnViaConversation(result) {
			c.logf("[handoff] 探测判定为生图轮次（SSE 无生图关键词）→ 转 conversation 轮询 conv=%s", result.ConversationID)
			result.ExpectGeneratedImages = true
			result.sawImageGenTool = true
			return true
		}
	}
	return false
}

// collectImagesViaPolling 生图收图统一入口。
//
// 官网实测：生图（imagegen/picture_v2/thinking 批量）图片经 GET /conversation 轮询下发
// （async_status 3/5→4），不走 WS；而预建的 WS 在长 SSE（模型思考）期间会空闲失效。
// 因此这里先关闭 WS（wsConn 为 nil 表示无需关闭），再轮询收齐所有图片（thinking 批量按
// batch_requests 张数收齐），最后统一定稿并汇总日志。
func (c *Client) collectImagesViaPolling(wsConn *websocket.Conn, result *ChatResult, opts ChatOptions, logReason string) {
	if wsConn != nil {
		_ = wsConn.Close()
	}
	if logReason != "" {
		c.logf("%s", logReason)
	}
	if err := c.pollConversationUntilDone(result, opts); err != nil {
		// 轮询超时不致命：已拿到的图片仍返回，仅记录。
		c.logf("[handoff] 生图轮询结束（未满足完成条件）: %v", err)
	}
	c.FinishImageGenWS(result, opts)
	c.MergeApplyAndEmitArtifacts(result, opts)
	switch {
	case result.HasDalleGeneratedOutput():
		c.logf("[artifact] 生图 file_id: %v", result.ImageFileIDs)
	case len(result.ImageFileIDs) > 0:
		c.logf("[image-poll] 无 DALL·E 产出（勿将用户参考图 file_id 当作生图结果）: %v", result.ImageFileIDs)
	default:
		c.logf("[image-poll] 轮询结束但未拾取到生图 file_id conv=%s", result.ConversationID)
		// 兜底：命中生图标记（如 t2uay3k）后转轮询，但官网卡住或模型最终改用文本作答时会 0 图。
		// 此时回退拉取 assistant 正文，避免整轮回复丢失（返回空）。
		if result.assistantFinalText == "" && result.ConversationID != "" {
			if raw, err := c.FetchConversationRaw(result.ConversationID); err == nil {
				var conv map[string]interface{}
				if json.Unmarshal(raw, &conv) == nil {
					if txt, mid := extractFinalAssistantText(conv); txt != "" {
						result.assistantFinalText = txt
						c.noteSandboxArtifactsFromText(txt, mid, result)
						c.logf("[image-poll] 未出图但取到 assistant 最终正文（len=%d），回退为文本回复", len(txt))
					}
				}
			}
		}
	}
}

// probeImageRouteFromSSE 诊断：区分 DALL·E async（应有 gen_id）与 Codex image_gen 容器路径（通常无 gen_id）。
// 检测到生图工具时自动设置 ExpectGeneratedImages=true，无需调用方提前开启 ForcePictureV2。
func (c *Client) probeImageRouteFromSSE(payload string, result *ChatResult, opts ChatOptions) {
	if result == nil {
		return
	}
	lower := strings.ToLower(payload)
	switch {
	case strings.Contains(lower, "image_gen_task_id") || strings.Contains(lower, "ghostrider"):
		if !result.sawImageGenTool {
			c.logf("[image-route] dalle_async（预期 conversation-update + gen_id）model=%s ForcePictureV2=%v", c.model, opts.ForcePictureV2)
		}
		result.sawImageGenTool = true
		result.ExpectGeneratedImages = true
	case strings.Contains(lower, `"name": "image_gen"`) || strings.Contains(lower, `"name":"image_gen"`):
		if !result.sawImageGenTool {
			c.logf("[image-route] codex_image_gen 工具（built-in image_gen，通常无 dalle.gen_id）model=%s", c.model)
		}
		result.sawImageGenTool = true
		result.ExpectGeneratedImages = true
	case strings.Contains(lower, "container.exec") && strings.Contains(lower, "imagegen"):
		if !result.sawImageGenTool {
			c.logf("[image-route] codex_container 读取 imagegen skill（thinking+picture_v2 常见路径，非 classic DALL·E）model=%s", c.model)
		}
		result.sawImageGenTool = true
		result.ExpectGeneratedImages = true
	case strings.Contains(lower, `"name": "dalle"`) || strings.Contains(lower, `"name":"dalle"`):
		c.logf("[image-route] dalle 工具调用 model=%s", c.model)
		result.sawImageGenTool = true
		result.ExpectGeneratedImages = true
	}
}

// imageTurnMarkers 生图标记：出现其一即可判定本轮为生图（thinking 批量或 classic）。
//   - batch_requests：thinking 把"多图"拆成的子请求（code 节点）
//   - image_asset_pointer / sediment://：生成图的 asset_pointer
//   - t2uay3k：图像生成工具 recipient 命名空间（占位节点常在早期出现，可提前判定）
//   - image_gen_task_id：DALL·E async 任务标志
//
// 注意：async_status 不在此列 —— thinking 文本轮次（尤其带附件走 async）同样会出现
// async_status=3/5，用它判生图会把文本轮次误判为生图并丢弃正文（历史 bug）。
var imageTurnMarkers = []string{"batch_requests", "image_asset_pointer", "sediment://", "t2uay3k", "image_gen_task_id"}

// probeImageTurnViaConversation 探测 GET /conversation，判断本轮是否为（thinking 批量）生图。
//
// 背景：thinking 模型的主 SSE 极短且不含生图关键词，生图与"带附件的文本轮次"都走 async
// （SSE [DONE] 后对话经 GET /conversation 下发）。二者仅凭 async_status 无法区分，必须看
// mapping 里是否出现真正的生图标记（imageTurnMarkers）。
//
// 判定策略（每 interval 轮询一次，先到者胜）：
//   - mapping 含生图标记 → 返回 true（生图轮次，调用方关 WS 转纯轮询收图）；
//   - 出现 assistant final 正文且无生图标记 → 文本轮次：直接把正文写入 result（并置
//     asyncTextResolved），返回 false，调用方据此跳过 WS catchup，避免空闲 WS 失效风险；
//   - 均未出现 → 继续轮询直至窗口结束（生图/长思考可能耗时较久，故窗口放宽到 90s，
//     但一旦命中任一信号即提前返回）。
//
// SSE [DONE] 后对话常尚未就绪（404 conversation_inaccessible），此处重试覆盖该窗口。
func (c *Client) probeImageTurnViaConversation(result *ChatResult) bool {
	if c == nil || result == nil || result.ConversationID == "" {
		return false
	}
	const maxWait = 90 * time.Second
	const interval = 3 * time.Second
	deadline := time.Now().Add(maxWait)
	attempt := 0
	for {
		attempt++
		raw, err := c.FetchConversationRaw(result.ConversationID)
		if err != nil {
			// 404/未就绪：对话刚创建常暂不可访问（async 轮次典型表现），重试。
			c.logf("[image-probe] #%d 对话暂不可访问（重试）: %v", attempt, err)
		} else {
			s := string(raw)
			for _, mk := range imageTurnMarkers {
				if strings.Contains(s, mk) {
					c.logf("[image-probe] #%d 命中生图标记 %q → 生图轮次", attempt, mk)
					return true
				}
			}
			var conv map[string]interface{}
			if json.Unmarshal(raw, &conv) == nil {
				// 出现"已结束"的 assistant 最终正文且无生图标记 → 文本轮次：就地取回正文，跳过 WS catchup。
				// 注意 extractFinalAssistantText 只认 end_turn/is_complete，避免把开场白误当最终答复。
				if txt, mid := extractFinalAssistantText(conv); txt != "" {
					result.assistantFinalText = txt
					result.asyncTextResolved = true
					// 顺带登记最终答复引用的沙箱产物（PDF/文件），供 handler 生成下载代理链接。
					c.noteSandboxArtifactsFromText(txt, mid, result)
					c.logf("[image-probe] #%d 已结束的 assistant 最终正文（无生图标记）→ 文本轮次（正文 len=%d，沙箱产物 %d）",
						attempt, len(txt), len(result.SandboxArtifacts))
					return false
				}
			}
		}
		if time.Now().After(deadline) {
			c.logf("[image-probe] 探测窗口结束（%d 次）未见生图标记 → 视为非生图", attempt)
			return false
		}
		time.Sleep(interval)
	}
}

// recoverFinalTextAfterStreamFailure 在 WS/流异常中断后尽量保住本轮结果。
//
// 典型场景：thinking + 附件上传后 stream_handoff 转 WS，本机 socks5/防火墙以 1006
// 掐断连接，但 chatgpt.com 会话里最终 JSON/正文已写完（网页可见）。此时应：
//  1) 若本地已有 final 正文 → 直接视为成功；
//  2) 否则轮询 GET /conversation，用 extractFinalAssistantText 取回已结束答复。
// 成功时返回 true，调用方不得再把原始 WS 错误抛给客户端。
func (c *Client) recoverFinalTextAfterStreamFailure(result *ChatResult, lastText *string, handler StreamHandler, cause error) bool {
	if c == nil || result == nil {
		return false
	}
	if strings.TrimSpace(result.assistantFinalText) != "" {
		c.logf("[handoff] 流失败但已有 final 正文（len=%d），忽略: %v", len(result.assistantFinalText), cause)
		return true
	}
	if lastText != nil && strings.TrimSpace(*lastText) != "" && result.bodyStreamFromSSE {
		result.assistantFinalText = *lastText
		c.logf("[handoff] 流失败但 SSE 已有正文（len=%d），忽略: %v", len(*lastText), cause)
		return true
	}
	if result.ConversationID == "" {
		return false
	}

	c.logf("[handoff] 流失败，改从 conversation 拉取最终正文: %v conv=%s", cause, result.ConversationID)
	const maxWait = 120 * time.Second
	const interval = 3 * time.Second
	deadline := time.Now().Add(maxWait)
	for attempt := 1; ; attempt++ {
		raw, err := c.FetchConversationRaw(result.ConversationID)
		if err != nil {
			c.logf("[handoff] conversation 恢复 #%d: %v", attempt, err)
		} else {
			var conv map[string]interface{}
			if json.Unmarshal(raw, &conv) == nil {
				if txt, mid := extractFinalAssistantText(conv); strings.TrimSpace(txt) != "" {
					result.assistantFinalText = txt
					result.asyncTextResolved = true
					c.noteSandboxArtifactsFromText(txt, mid, result)
					if lastText != nil {
						c.emitBodyFull(result, lastText, txt, "final", handler)
					}
					c.logf("[handoff] conversation 已恢复最终正文 len=%d（#%d）", len(txt), attempt)
					return true
				}
			}
		}
		if time.Now().After(deadline) {
			c.logf("[handoff] conversation 恢复超时（%d 次），无法取得最终正文", attempt)
			return false
		}
		time.Sleep(interval)
	}
}

// extractFinalAssistantText 取回本轮"已结束"的 assistant 最终正文及其消息 id。
//
// 关键：只认 end_turn=true（或 metadata.is_complete=true）的节点。thinking 轮次常先流式
// 一段"开场白"（如"我这就帮你生成…"，end_turn=false）再异步继续干活（代码执行/生图/最终答复）。
// 若不看 end_turn 就取第一段文本，会把开场白误当最终答复、丢掉真正的结果（PDF 链接等）。
// 一轮可能有多个已结束节点（罕见），取 create_time 最大者；无则返回空。
func extractFinalAssistantText(conv map[string]interface{}) (text, msgID string) {
	mapping, _ := conv["mapping"].(map[string]interface{})
	var bestTime float64 = -1
	for _, nodeRaw := range mapping {
		node, ok := nodeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := node["message"].(map[string]interface{})
		if !ok {
			continue
		}
		if getNestedString(msg, "author", "role") != "assistant" {
			continue
		}
		// 只认已结束的终结节点（end_turn 或 is_complete）。
		et, _ := msg["end_turn"].(bool)
		ic := false
		if md, ok := msg["metadata"].(map[string]interface{}); ok {
			ic, _ = md["is_complete"].(bool)
		}
		if !et && !ic {
			continue
		}
		content, ok := msg["content"].(map[string]interface{})
		if !ok {
			continue
		}
		if ct, _ := content["content_type"].(string); ct != "text" {
			continue
		}
		parts, _ := content["parts"].([]interface{})
		var sb strings.Builder
		for _, p := range parts {
			if s, _ := p.(string); strings.TrimSpace(s) != "" {
				sb.WriteString(s)
			}
		}
		txt := sb.String()
		if strings.TrimSpace(txt) == "" {
			continue
		}
		if ctime, _ := msg["create_time"].(float64); ctime >= bestTime {
			bestTime = ctime
			text = txt
			msgID, _ = msg["id"].(string)
		}
	}
	return text, msgID
}

// noteSandboxArtifactsFromText 从最终答复文本里提取沙箱产物路径（/mnt/data/xxx.ext），
// 登记到 result.SandboxArtifacts，使 handler 能生成 /api/pdf/proxy 下载代理链接。
// 只取最终答复里引用的文件（如下载链接指向的 PDF），避免把中间渲染/预览文件（page-1.png 等）算进来。
func (c *Client) noteSandboxArtifactsFromText(text, msgID string, result *ChatResult) {
	if result == nil {
		return
	}
	if msgID == "" {
		msgID = result.LastAssistantMsgID
	}
	seen := make(map[string]bool)
	for _, a := range result.SandboxArtifacts {
		seen[a.SandboxPath] = true
	}
	for _, p := range sandboxFileRe.FindAllString(text, -1) {
		if !isValidSandboxPath(p) || seen[p] {
			continue
		}
		seen[p] = true
		result.SandboxArtifacts = append(result.SandboxArtifacts, SandboxArtifact{
			MessageID:   msgID,
			SandboxPath: p,
			FileName:    p[strings.LastIndex(p, "/")+1:],
		})
	}
	result.PDFArtifacts = filterPDFArtifacts(result.SandboxArtifacts)
}
