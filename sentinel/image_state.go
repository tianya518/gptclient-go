package sentinel

// image_state.go —— 生图收图过程中的状态判定（只读查询，不改协议）。
//
// 汇集"是否可跳过 WS 长等待 / 是否收齐 / 是否可 idle 退出 / 各槽位是否定稿"等判断，
// 供 chat_stream.go 的分发路由与 chat_ws.go 的收尾循环使用。

import (
	"fmt"
	"time"
)

// CanSkipImageWSAfterSSE HTTP SSE 结束后是否可跳过 WS 长等待（单图可跳过；多图需 async 完成或全部槽位已填满）。
func (result *ChatResult) CanSkipImageWSAfterSSE() bool {
	if !result.HasDalleGeneratedOutput() {
		return false
	}
	if result.imageAsyncTaskPending > 0 || result.imageAsyncTaskActive {
		return false
	}
	if result.imageGenAsyncCompleteSeen || result.imageGenConvAsyncStatusDone {
		return true
	}
	n := len(result.imageSlots)
	if n == 1 {
		return true
	}
	if n >= 2 && result.allImageSlotsPopulated() {
		return true
	}
	return false
}

func (result *ChatResult) allImageSlotsPopulated() bool {
	if len(result.imageSlots) == 0 {
		return false
	}
	for _, s := range result.imageSlots {
		if s == nil || s.FileID == "" {
			return false
		}
	}
	return true
}

// ImageGenDiagSummary 生图诊断摘要（供 server 日志）。
func (result *ChatResult) ImageGenDiagSummary() string {
	if result == nil {
		return "slots=0"
	}
	return fmt.Sprintf("slots=%d saw_tool=%v picture_v2=%v has_dalle=%v",
		len(result.imageSlots), result.sawImageGenTool, result.pictureV2Path, result.HasDalleGeneratedOutput())
}

// HasDalleGeneratedOutput 是否已有 WS 生图产出（含 picture_v2 无 gen_id 的 image_gen 出图）。
func (result *ChatResult) HasDalleGeneratedOutput() bool {
	for _, s := range result.imageSlots {
		if s == nil || s.FileID == "" {
			continue
		}
		if s.GenID != "" {
			return true
		}
		if result.allowPictureV2ImageWithoutGenID(s.FileID) {
			return true
		}
	}
	return false
}

// AllImageSlotsFinal 所有图位是否已定稿。
func (result *ChatResult) AllImageSlotsFinal() bool {
	if len(result.imageSlots) == 0 {
		return false
	}
	for _, s := range result.imageSlots {
		if s == nil || !s.Final {
			return false
		}
	}
	return true
}

// ImageGenIdleDuration 无新活动后等待多久再结束 WS（网页端修图/多轮 async 需更久）。
func ImageGenIdleDuration(result *ChatResult) time.Duration {
	if result == nil {
		return 15 * time.Second
	}
	if result.imageAsyncTaskPending > 0 || result.imageAsyncTaskActive {
		return 25 * time.Second
	}
	if len(result.imageSlots) >= 2 && !result.AllImageSlotsFinal() {
		return 30 * time.Second
	}
	return 15 * time.Second
}

// CanImageGenIdleExit 是否允许结束生图 WS（优先服务端 complete / turn [DONE]，辅以 idle）。
func (result *ChatResult) CanImageGenIdleExit() bool {
	if result == nil || result.lastImageGenActivityAt == 0 {
		return false
	}
	hasDalle := result.HasDalleGeneratedOutput()
	// imagegen skill 批量路径：sawImageGenTool=true，图片全部来自 conversation 轮询，无 dalle gen_id
	hasImagegenOutput := result.sawImageGenTool && result.allImageSlotsPopulated()
	if !hasDalle && !hasImagegenOutput {
		return false
	}
	result.MaybeClearStaleImageAsyncPending()
	if result.imageAsyncTaskPending > 0 {
		return false
	}
	since := time.Since(time.Unix(0, result.lastImageGenActivityAt))
	// imagegen skill 路径：async_status=4 且所有槽位已填 → 可退出
	if result.imageGenConvAsyncStatusDone && hasImagegenOutput {
		return true
	}
	// 经典路径：async_status=4
	if result.imageGenConvAsyncStatusDone && hasDalle {
		return true
	}
	if result.imageGenAsyncCompleteSeen {
		return since >= 2*time.Second
	}
	if result.imageGenTurnDone && (hasDalle || hasImagegenOutput) {
		return since >= 2*time.Second
	}
	return since >= ImageGenIdleDuration(result)
}
