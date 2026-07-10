package sentinel

import (
	"fmt"
	"strings"
	"time"
)

// MaybeClearStaleImageAsyncPending 有图但长期未收到 complete 时，解除 pending 避免永久卡住。
func (result *ChatResult) MaybeClearStaleImageAsyncPending() bool {
	if result == nil || result.imageAsyncTaskPending <= 0 || result.imageGenAsyncCompleteSeen {
		return false
	}
	if !result.HasDalleGeneratedOutput() {
		return false
	}
	since := time.Since(time.Unix(0, result.lastImageGenActivityAt))
	if since < 20*time.Second {
		return false
	}
	result.imageAsyncTaskPending = 0
	result.imageAsyncTaskActive = false
	return true
}

// ImageGenExitBlockReason 当前为何还不能结束 WS（用于诊断日志）。
func (result *ChatResult) ImageGenExitBlockReason() string {
	if result == nil {
		return "blocking=nil"
	}
	if !result.HasDalleGeneratedOutput() {
		return "blocking=no_dalle_image_yet"
	}
	if result.lastImageGenActivityAt == 0 {
		return "blocking=no_image_activity_ts"
	}
	since := time.Since(time.Unix(0, result.lastImageGenActivityAt))
	if result.imageAsyncTaskPending > 0 {
		return fmt.Sprintf("blocking=async_pending(%d,active=%v) idleSinceImg=%.1fs", result.imageAsyncTaskPending, result.imageAsyncTaskActive, since.Seconds())
	}
	need := ImageGenIdleDuration(result)
	if result.imageGenAsyncCompleteSeen || result.imageGenConvAsyncStatusDone {
		need = 3 * time.Second
		if since < need {
			return fmt.Sprintf("blocking=post_complete_idle(%.1fs/%.0fs convStatus=%v)",
				since.Seconds(), need.Seconds(), result.imageGenConvAsyncStatusDone)
		}
		return "ok"
	}
	if result.imageGenTurnDone {
		return fmt.Sprintf("blocking=turn_done_but_async_may_continue idleSinceImg=%.1fs", since.Seconds())
	}
	if since < need {
		return fmt.Sprintf("blocking=idle_wait(%.1fs/%.0fs)", since.Seconds(), need.Seconds())
	}
	return "ok"
}

func summarizeConvUpdatePayload(payload map[string]interface{}) string {
	if payload == nil {
		return ""
	}
	updateType, _ := payload["update_type"].(string)
	uc, _ := payload["update_content"].(map[string]interface{})
	if uc == nil {
		return fmt.Sprintf("type=%s", updateType)
	}
	parts := []string{fmt.Sprintf("type=%s", updateType)}
	if tid, ok := uc["async_task_id"].(string); ok && tid != "" {
		if len(tid) > 12 {
			tid = tid[:12] + "…"
		}
		parts = append(parts, "task="+tid)
	}
	if _, ok := uc["message"]; ok {
		parts = append(parts, "msg=1")
	}
	if msgs, ok := uc["messages"].([]interface{}); ok {
		parts = append(parts, fmt.Sprintf("msgs=%d", len(msgs)))
	}
	if st, ok := uc["conversation_async_status"].(float64); ok {
		parts = append(parts, fmt.Sprintf("async_status=%d", int(st)))
	}
	return strings.Join(parts, " ")
}

func (c *Client) logImageGenDiag(result *ChatResult, tag string) {
	if result == nil {
		return
	}
	sinceImg := -1.0
	if result.lastImageGenActivityAt > 0 {
		sinceImg = time.Since(time.Unix(0, result.lastImageGenActivityAt)).Seconds()
	}
	c.logf("[image-ws][diag] %s pending=%d active=%v complete=%v convStatus=%v turnDone=%v slots=%d dalle=%v idleSinceImg=%.1fs block=%s",
		tag,
		result.imageAsyncTaskPending, result.imageAsyncTaskActive,
		result.imageGenAsyncCompleteSeen, result.imageGenConvAsyncStatusDone, result.imageGenTurnDone,
		len(result.imageSlots), result.HasDalleGeneratedOutput(), sinceImg,
		result.ImageGenExitBlockReason(),
	)
}
