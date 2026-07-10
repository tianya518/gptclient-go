package sentinel

// image_poll.go —— 生图收图的 GET /conversation 轮询实现。
//
// 官网（chatgpt.com）当前生图协议要点（详见 docs/IMAGE_FLOW_CAPTURE.md）：
//   1. 主 POST /f/conversation 的 SSE 极短：thinking 批量生图时甚至不含任何生图关键词，
//      只给出 stream_handoff；图片一律经 GET /conversation/{id} 轮询下发，不走 WebSocket。
//   2. 轮询响应顶层 async_status：3=生成中、5=思考/排队、4=完成；字段缺失=同步已完成（旧路径）。
//   3. thinking 会把"分别生成 N 张"拆成一个 code 节点里的 batch_requests:[...]（长度=期望张数），
//      随后每张图各占一个 tool(multimodal_text) 节点，asset_pointer 前缀为 sediment://（新）或
//      file-service://（旧）。同一张图可能被引用多次，需按 file_id 去重。
//
// 收图路径：pollConversationUntilDone → 循环 pollConversationForImages（解析 async_status +
// batch 张数 + 逐张 file_id）→ async 完成后 drainRemainingImages 按期望张数补收 → 定稿。
// image_handoff.go 的 collectImagesViaPolling 是外部统一入口（关 WS + 调用本文件 + 定稿汇总）。

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

func (c *Client) tryFetchGeneratedImagesFromConversation(result *ChatResult, opts ChatOptions) {
	if c == nil || result == nil || result.ConversationID == "" {
		return
	}
	// 经典 DALL·E 路径（有 gen_id）：已有产出则跳过（WS 已覆盖）
	// imagegen skill 路径（无 gen_id，sawImageGenTool=true）：始终允许轮询拿增量图片
	if result.HasDalleGeneratedOutput() && !result.sawImageGenTool {
		return
	}
	c.pollConversationForImages(result, opts)
}

// pollConversationForImages 对 imagegen skill（批量生图，无 dalle gen_id）路径执行
// conversation API 轮询，逐张收集 file_id，直到全部就绪或超时。
// 官网实测：6张图约需 4~7 分钟，期间浏览器持续以约 10s/次频率轮询 GET /conversation/{id}。
func (c *Client) pollConversationForImages(result *ChatResult, opts ChatOptions) {
	if c == nil || result == nil || result.ConversationID == "" {
		return
	}
	raw, err := c.FetchConversationRaw(result.ConversationID)
	if err != nil {
		c.logf("[image-poll] conversation fetch #%d: %v", result.imageGenConvPollCount+1, err)
		return
	}
	if os.Getenv("SENTINEL_DUMP_CONV") != "" {
		fn := "conv_dump_" + result.ConversationID + ".json"
		if werr := os.WriteFile(fn, raw, 0644); werr == nil {
			c.logf("[image-poll][dump] 已写 %s (%d bytes)", fn, len(raw))
		}
	}
	var conv map[string]interface{}
	if err := json.Unmarshal(raw, &conv); err != nil {
		c.logf("[image-poll] conversation parse: %v", err)
		return
	}

	// 顶层 async_status 语义：3=生成中、5=思考/排队（均视为进行中，继续轮询）、4=完成；
	// 字段缺失=同步写入模式（旧版/非 async 对话），图片已在 mapping 中，直接标记完成。
	if asyncStatus, ok := conv["async_status"].(float64); ok {
		status := int(asyncStatus)
		c.logf("[image-poll] async_status=%d poll#%d", status, result.imageGenConvPollCount+1)
		if status == 4 {
			result.imageGenConvAsyncStatusDone = true
			result.imageAsyncTaskPending = 0
			result.imageAsyncTaskActive = false
		}
	} else {
		// async_status 字段不存在：图片节点同步写入 mapping，直接标记完成
		c.logf("[image-poll] async_status 字段不存在（同步写入模式），标记为 done，poll#%d", result.imageGenConvPollCount+1)
		result.imageGenConvAsyncStatusDone = true
	}

	mapping, _ := conv["mapping"].(map[string]interface{})
	// 期望张数：thinking 批量生图会在某个 code 节点内下发 batch_requests:[...]，长度=张数。
	// 取整轮 mapping 的最大值（多轮/重试时以最大批次为准）。
	if bc := countBatchRequests(mapping); bc > result.expectedImageCount {
		result.expectedImageCount = bc
		c.logf("[image-poll] 检测到 batch_requests 期望张数=%d", bc)
	}
	prevCount := len(result.imageSlots)
	n := 0
	for _, nodeRaw := range mapping {
		node, ok := nodeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := node["message"].(map[string]interface{})
		if !ok {
			continue
		}
		c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "conv_poll")
		n++
	}

	result.imageGenConvPollCount++
	result.imageGenConvLastPollAt = time.Now().UnixNano()
	newCount := len(result.imageSlots)
	c.logf("[image-poll] poll#%d 扫描=%d 节点 slots=%d→%d dalle=%v asyncDone=%v",
		result.imageGenConvPollCount, n, prevCount, newCount,
		result.HasDalleGeneratedOutput(), result.imageGenConvAsyncStatusDone)
}

// countBatchRequests 扫描 mapping，找出 code 节点里 batch_requests 数组的最大长度。
// thinking 批量生图（如"分别生成6张不同风格"）会把请求拆成 N 条 batch_requests，N 即期望张数。
func countBatchRequests(mapping map[string]interface{}) int {
	best := 0
	for _, nodeRaw := range mapping {
		node, ok := nodeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := node["message"].(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := msg["content"].(map[string]interface{})
		if !ok {
			continue
		}
		if ct, _ := content["content_type"].(string); ct != "code" {
			continue
		}
		text, _ := content["text"].(string)
		if text == "" || !strings.Contains(text, "batch_requests") {
			continue
		}
		if n := parseBatchRequestsLen(text); n > best {
			best = n
		}
	}
	return best
}

// parseBatchRequestsLen 从 code 文本中解析 batch_requests 数组长度（优先 JSON，失败回退计数）。
func parseBatchRequestsLen(text string) int {
	var obj struct {
		BatchRequests []json.RawMessage `json:"batch_requests"`
	}
	if err := json.Unmarshal([]byte(text), &obj); err == nil && len(obj.BatchRequests) > 0 {
		return len(obj.BatchRequests)
	}
	// 回退：batch_requests 内每条子请求通常含 "prompt" 字段，用其出现次数近似张数。
	if idx := strings.Index(text, "batch_requests"); idx >= 0 {
		return strings.Count(text[idx:], "\"prompt\"")
	}
	return 0
}

// pollConversationUntilDone 在 WS 断连后，以 10s 间隔持续轮询 conversation API，
// 直到 async_status=4（全部完成）或超过最大等待时间（10分钟）。
// 用于 imagegen skill 批量生图路径：WS 断开后不报错，转为 HTTP 轮询保底。
func (c *Client) pollConversationUntilDone(result *ChatResult, opts ChatOptions) error {
	const maxWait = 10 * time.Minute
	const pollInterval = 10 * time.Second
	deadline := time.Now().Add(maxWait)

	// 轮询路径：强制开启 ExpectGeneratedImages，确保解析逻辑不会被过早 guard 截断
	if result != nil && !result.ExpectGeneratedImages {
		c.logf("[image-poll] ExpectGeneratedImages=false（WS 断连前未来得及设置），强制开启")
		result.ExpectGeneratedImages = true
	}
	// 进入轮询即认定本轮为生图：thinking 批量路径图片节点无 dalle.gen_id，
	// 需 sawImageGenTool=true 才会在 tryNoteGeneratedImagesFromMessage 中被接受。
	if result != nil {
		result.sawImageGenTool = true
	}
	c.logf("[image-poll] 开始 conversation 轮询收图，最长等待 %.0f 分钟", maxWait.Minutes())
	for time.Now().Before(deadline) {
		c.pollConversationForImages(result, opts)
		c.MergeApplyAndEmitArtifacts(result, opts)

		if result.imageGenConvAsyncStatusDone {
			// async_status=4：图片节点可能还在陆续写入 mapping（有时晚于 async_status 更新）。
			// 多图（batch_requests=N）时逐张收齐，直到 file_id 数达到期望张数或补收超时。
			c.drainRemainingImages(result, opts)
			c.logf("[image-poll] 轮询完成：async_status=done，期望=%d 实收 slots=%d file_ids=%d",
				result.expectedImageCount, len(result.imageSlots), len(result.ImageFileIDs))
			return nil
		}
		c.logf("[image-poll] 等待下次轮询... slots=%d 期望=%d asyncDone=%v",
			len(result.imageSlots), result.expectedImageCount, result.imageGenConvAsyncStatusDone)
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("imagegen poll 超时（%.0f 分钟），slots=%d", maxWait.Minutes(), len(result.imageSlots))
}

// drainRemainingImages 在 async_status 完成后补收多图：图片节点常晚于 async_status 写入 mapping。
// 若已知期望张数（batch_requests=N），持续补收直到收齐 N 张或补收窗口用尽。
func (c *Client) drainRemainingImages(result *ChatResult, opts ChatOptions) {
	const grace = 45 * time.Second // 补收窗口（多图逐张下发，给足时间）
	const step = 3 * time.Second
	graceDeadline := time.Now().Add(grace)
	for {
		collected := len(result.ImageFileIDs)
		want := result.expectedImageCount
		if want > 0 && collected >= want {
			c.logf("[image-poll] 已按期望收齐 %d/%d 张", collected, want)
			return
		}
		if time.Now().After(graceDeadline) {
			c.logf("[image-poll] 补收窗口结束：期望=%d 实收=%d", want, collected)
			return
		}
		time.Sleep(step)
		c.pollConversationForImages(result, opts)
		c.MergeApplyAndEmitArtifacts(result, opts)
		if len(result.ImageFileIDs) > collected {
			// 仍在增长：延长补收窗口，直到稳定
			graceDeadline = time.Now().Add(grace)
			c.logf("[image-poll] 新增图片 %d→%d（期望=%d），延长补收", collected, len(result.ImageFileIDs), want)
		}
		if want == 0 {
			// 期望未知（非 batch 路径）：不再增长即认为收齐
			if len(result.ImageFileIDs) == collected {
				c.logf("[image-poll] 期望未知且图片数稳定=%d，结束补收", collected)
				return
			}
		}
	}
}
