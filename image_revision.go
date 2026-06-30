package sentinel

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 生图版本推送策略（请求 artifact_image_revisions）。
const (
	ImageRevisionAll           = "all"             // 每个 file_id 各推一次（含中间稿）
	ImageRevisionLatestPerSlot = "latest_per_slot" // 按槽位只推最新，旧图发 superseded
	ImageRevisionFinalOnly     = "final_only"      // 槽位 idle 结束后推最终版
)

// GeneratedImageSlot 一个「图位」（图1/图2…）及其修订历史。
type GeneratedImageSlot struct {
	SlotIndex  int
	GenID      string
	MessageID  string
	FileID     string
	Revision   int
	FileHistory []string
	Final      bool
}

// StreamEvent 扩展字段见 stream_events.go（SlotIndex、Revision、GenID 等）。

func (cfg ArtifactStreamConfig) imageRevisionMode() string {
	n := cfg.normalized()
	if n.ImageRevisions == "" {
		return ImageRevisionLatestPerSlot
	}
	return n.ImageRevisions
}

func (result *ChatResult) ensureImageSlots() {
	if result.imageSlots == nil {
		result.imageSlots = make(map[string]*GeneratedImageSlot)
	}
	if result.emittedArtifacts == nil {
		result.emittedArtifacts = make(map[string]bool)
	}
}

func slotMapKey(genID, messageID string) string {
	if genID != "" {
		return "gen:" + genID
	}
	if messageID != "" {
		return "msg:" + messageID
	}
	return ""
}

func (result *ChatResult) findSlotByParent(parentGenID string) *GeneratedImageSlot {
	if parentGenID == "" {
		return nil
	}
	for _, s := range result.imageSlots {
		if s.GenID == parentGenID {
			return s
		}
	}
	return nil
}

func (result *ChatResult) assignImageSlot(genID, messageID, parentGenID string) *GeneratedImageSlot {
	result.ensureImageSlots()
	if k := slotMapKey(genID, messageID); k != "" {
		if s, ok := result.imageSlots[k]; ok {
			return s
		}
	}
	if parent := result.findSlotByParent(parentGenID); parent != nil {
		return parent
	}
	// 新槽位
	idx := len(result.imageSlots) + 1
	s := &GeneratedImageSlot{SlotIndex: idx, GenID: genID, MessageID: messageID}
	k := slotMapKey(genID, messageID)
	if k == "" {
		k = fmt.Sprintf("slot:%d", idx)
	}
	result.imageSlots[k] = s
	return s
}

// ParsedGeneratedImage 从 WS message / part 解析出的生图条目。
type ParsedGeneratedImage struct {
	FileID      string
	MessageID   string
	GenID       string
	ParentGenID string
	EditOp      string
	Width       int
	Height      int
}

func parseGeneratedImagesFromMessage(msg map[string]interface{}) []ParsedGeneratedImage {
	msgID, _ := msg["id"].(string)
	content, _ := msg["content"].(map[string]interface{})
	parts, _ := content["parts"].([]interface{})
	var out []ParsedGeneratedImage
	appendPart := func(partMap map[string]interface{}) {
		if partMap["content_type"] != "image_asset_pointer" {
			return
		}
		ap, _ := partMap["asset_pointer"].(string)
		fileID := extractFileID(ap)
		if fileID == "" {
			return
		}
		p := ParsedGeneratedImage{FileID: fileID, MessageID: msgID}
		if w, ok := partMap["width"].(float64); ok {
			p.Width = int(w)
		}
		if h, ok := partMap["height"].(float64); ok {
			p.Height = int(h)
		}
		if meta, ok := partMap["metadata"].(map[string]interface{}); ok {
			if dalle, ok := meta["dalle"].(map[string]interface{}); ok {
				p.GenID, _ = dalle["gen_id"].(string)
				if pg, ok := dalle["parent_gen_id"].(string); ok {
					p.ParentGenID = pg
				}
				p.EditOp, _ = dalle["edit_op"].(string)
			}
		}
		out = append(out, p)
	}
	for _, part := range parts {
		partMap, ok := part.(map[string]interface{})
		if !ok {
			continue
		}
		appendPart(partMap)
	}
	if ct, _ := content["content_type"].(string); ct == "image_asset_pointer" {
		appendPart(content)
	}
	return out
}

func (result *ChatResult) isUserReferenceFile(fileID string) bool {
	return result != nil && result.userReferenceFileIDs != nil && result.userReferenceFileIDs[fileID]
}

// allowPictureV2ImageWithoutGenID picture_v2 / image_gen 工具产出常无 dalle.gen_id，但仍为有效生图。
func (result *ChatResult) allowPictureV2ImageWithoutGenID(fileID string) bool {
	if fileID == "" || result == nil || result.isUserReferenceFile(fileID) {
		return false
	}
	return result.pictureV2Path || result.sawImageGenTool || result.ImageTaskID != ""
}

func (c *Client) tryNoteGeneratedImagesFromMessage(msg map[string]interface{}, result *ChatResult, opts ChatOptions, via string) {
	if c == nil || result == nil || !result.ExpectGeneratedImages {
		return
	}
	role := getNestedString(msg, "author", "role")
	imgs := parseGeneratedImagesFromMessage(msg)
	if len(imgs) == 0 {
		return
	}
	switch role {
	case "assistant":
		// picture_v2 / image_gen 常把图放在 assistant multimodal_text
	case "tool":
		// classic DALL·E 出图常在 tool 消息（含 dalle.gen_id）
		accepted := false
		for _, img := range imgs {
			if img.GenID != "" || result.allowPictureV2ImageWithoutGenID(img.FileID) {
				accepted = true
				break
			}
		}
		if !accepted {
			return
		}
	default:
		return
	}
	msgID, _ := msg["id"].(string)
	for _, img := range imgs {
		if img.MessageID == "" {
			img.MessageID = msgID
		}
		if c != nil && img.GenID == "" {
			c.logf("[image-parse] %s image file=%s gen_id=空 via=%s allow_no_gen=%v",
				role, img.FileID, via, result.allowPictureV2ImageWithoutGenID(img.FileID))
		} else if c != nil && img.GenID != "" {
			c.logf("[image-parse] %s image file=%s gen_id=%s via=%s", role, img.FileID, img.GenID, via)
		}
		c.noteGeneratedImageRevision(result, opts, img, via)
	}
}

func (c *Client) tryFetchGeneratedImagesFromConversation(result *ChatResult, opts ChatOptions) {
	if c == nil || result == nil || result.ConversationID == "" || result.HasDalleGeneratedOutput() {
		return
	}
	raw, err := c.FetchConversationRaw(result.ConversationID)
	if err != nil {
		c.logf("[image-ws][fallback] conversation fetch: %v", err)
		return
	}
	var conv map[string]interface{}
	if err := json.Unmarshal(raw, &conv); err != nil {
		c.logf("[image-ws][fallback] conversation parse: %v", err)
		return
	}
	mapping, _ := conv["mapping"].(map[string]interface{})
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
		c.tryNoteGeneratedImagesFromMessage(msg, result, opts, "conv_fetch")
		n++
	}
	c.logf("[image-ws][fallback] conversation fetch 扫描 %d 节点 slots=%d dalle=%v",
		n, len(result.imageSlots), result.HasDalleGeneratedOutput())
}

func (c *Client) noteGeneratedImageRevision(result *ChatResult, opts ChatOptions, p ParsedGeneratedImage, wsUpdateType string) {
	if p.FileID == "" || result == nil || !result.ExpectGeneratedImages {
		return
	}
	// 经典 DALL·E 带 gen_id；picture_v2/image_gen 工具常无 gen_id，但仍为有效产出
	if p.GenID == "" && wsUpdateType != "finalize" {
		if !result.allowPictureV2ImageWithoutGenID(p.FileID) {
			return
		}
		if c != nil {
			c.logf("[image-ws][img] picture_v2 无 gen_id 仍接受 file=%s via=%s", p.FileID, wsUpdateType)
		}
	}
	if !imageFileIDSeen(result.ImageFileIDs, p.FileID) {
		result.ImageFileIDs = append(result.ImageFileIDs, p.FileID)
	}
	result.ImageFileID = p.FileID

	emitKey := "img:" + p.FileID
	if result.emittedArtifacts[emitKey] {
		return
	}

	slot := result.assignImageSlot(p.GenID, p.MessageID, p.ParentGenID)
	if p.MessageID != "" && slot.MessageID == "" {
		slot.MessageID = p.MessageID
	}
	if p.GenID != "" && slot.GenID == "" {
		slot.GenID = p.GenID
	}

	var prevFileID string
	if len(slot.FileHistory) > 0 {
		prevFileID = slot.FileHistory[len(slot.FileHistory)-1]
	}
	if prevFileID == p.FileID {
		return
	}

	now := time.Now().UnixNano()
	result.lastImageAddedAt = now
	result.lastImageGenActivityAt = now

	slot.Revision++
	slot.FileHistory = append(slot.FileHistory, p.FileID)
	slot.FileID = p.FileID
	slot.Final = false

	// 诊断：新图修订（重复 file_id 已在上方 return）
	if c != nil {
		c.logf("[image-ws][img] %s slot=%d rev=%d gen=%s file=%s", wsUpdateType, slot.SlotIndex, slot.Revision, p.GenID, p.FileID)
	}

	mode := opts.Artifacts.imageRevisionMode()
	cfg := opts.Artifacts.normalized()

	switch mode {
	case ImageRevisionAll:
		result.emittedArtifacts[emitKey] = true
		c.emitGeneratedImageEvent(cfg, result, p, slot, wsUpdateType, false, "")

	case ImageRevisionLatestPerSlot:
		if prevFileID != "" && prevFileID != p.FileID {
			sup := StreamEvent{
				Event:       StreamEventArtifactSuperseded,
				Kind:        "generated_image",
				SlotIndex:   slot.SlotIndex,
				Revision:    slot.Revision - 1,
				GenID:       slot.GenID,
				MessageID:   slot.MessageID,
				FileID:      prevFileID,
				UpdateType:  wsUpdateType,
			}
			if cfg.BuildImageURL != nil {
				sup.URL = cfg.BuildImageURL(prevFileID)
				if result.ConversationID != "" {
					sup.URL = patchProxyConvID(sup.URL, result.ConversationID)
				}
			}
			cfg.emit(sup)
		}
		result.emittedArtifacts[emitKey] = true
		c.emitGeneratedImageEvent(cfg, result, p, slot, wsUpdateType, false, prevFileID)

	case ImageRevisionFinalOnly:
		// 仅记录，在 FinalizeImageGenSlots 推送
		return
	}
}

func (c *Client) emitGeneratedImageEvent(cfg ArtifactStreamConfig, result *ChatResult, p ParsedGeneratedImage, slot *GeneratedImageSlot, wsUpdateType string, isFinal bool, supersedes string) {
	evBase := StreamEvent{
		Event:            StreamEventArtifact,
		Kind:             "generated_image",
		Index:            slot.SlotIndex,
		SlotIndex:        slot.SlotIndex,
		Revision:         slot.Revision,
		GenID:            p.GenID,
		MessageID:        p.MessageID,
		ParentGenID:      p.ParentGenID,
		FileID:           p.FileID,
		UpdateType:       wsUpdateType,
		IsFinal:          isFinal,
		SupersedesFileID: supersedes,
		MimeType:         "image/png",
		Name:             fmt.Sprintf("generated_slot%d_rev%d.png", slot.SlotIndex, slot.Revision),
	}
	if cfg.BuildImageURL != nil {
		evBase.URL = cfg.BuildImageURL(p.FileID)
	}
	if result != nil && result.ConversationID != "" {
		evBase.URL = patchProxyConvID(evBase.URL, result.ConversationID)
	}

	switch cfg.Delivery {
	case ArtifactDeliveryURL:
		cfg.emit(evBase)
		return
	}
	data, mime, err := c.DownloadFileByFileID(result.ConversationID, p.FileID)
	if err != nil {
		evBase.Error = err.Error()
		cfg.emit(evBase)
		return
	}
	evBase.SizeBytes = len(data)
	if mime != "" {
		evBase.MimeType = mime
	}
	c.emitArtifactBytes(cfg, evBase, data)
}

// FinalizeImageGenSlots 生图 WS 空闲结束时：final_only 推送，并标记各槽位 is_final。
func (c *Client) FinalizeImageGenSlots(result *ChatResult, opts ChatOptions) {
	if result == nil || !result.ExpectGeneratedImages {
		return
	}
	cfg := opts.Artifacts.normalized()
	mode := cfg.imageRevisionMode()

	for _, slot := range result.imageSlots {
		if slot == nil || slot.FileID == "" {
			continue
		}
		slot.Final = true
		if mode != ImageRevisionFinalOnly {
			cfg.emit(StreamEvent{
				Event:       StreamEventArtifactSlotFinal,
				Kind:        "generated_image",
				SlotIndex:   slot.SlotIndex,
				Revision:    slot.Revision,
				GenID:       slot.GenID,
				MessageID:   slot.MessageID,
				FileID:      slot.FileID,
				IsFinal:     true,
				Total:       len(result.imageSlots),
			})
			continue
		}
		emitKey := "img:" + slot.FileID
		if result.emittedArtifacts[emitKey] {
			continue
		}
		result.emittedArtifacts[emitKey] = true
		p := ParsedGeneratedImage{
			FileID:    slot.FileID,
			MessageID: slot.MessageID,
			GenID:     slot.GenID,
		}
		c.emitGeneratedImageEvent(cfg, result, p, slot, "finalize", true, "")
	}
}

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
	if result == nil || !result.HasDalleGeneratedOutput() || result.lastImageGenActivityAt == 0 {
		return false
	}
	result.MaybeClearStaleImageAsyncPending()
	if result.imageAsyncTaskPending > 0 {
		return false
	}
	since := time.Since(time.Unix(0, result.lastImageGenActivityAt))
	// 网页端完成：set-conversation-async-status=4（已有图即可结束，勿再等 ReadMessage 60s）
	if result.imageGenConvAsyncStatusDone && result.HasDalleGeneratedOutput() {
		return true
	}
	if result.imageGenAsyncCompleteSeen {
		return since >= 2*time.Second
	}
	// turn topic 已 [DONE] 且已有图：单轮生图结束，不必再等 15s idle（否则 ReadMessage 被 pong 续期卡住）
	if result.imageGenTurnDone && result.HasDalleGeneratedOutput() {
		return since >= 2*time.Second
	}
	// 不用 turnDone：正文流 [DONE] 常早于生图 conversation-update 结束
	return since >= ImageGenIdleDuration(result)
}

// RebuildImageFileIDsFromSlots 按槽位顺序刷新 ImageFileIDs（最终每槽最新 file_id）。
func (result *ChatResult) RebuildImageFileIDsFromSlots() {
	if len(result.imageSlots) == 0 {
		return
	}
	slots := make([]*GeneratedImageSlot, 0, len(result.imageSlots))
	for _, s := range result.imageSlots {
		slots = append(slots, s)
	}
	// 按 SlotIndex 排序
	for i := 0; i < len(slots); i++ {
		for j := i + 1; j < len(slots); j++ {
			if slots[j].SlotIndex < slots[i].SlotIndex {
				slots[i], slots[j] = slots[j], slots[i]
			}
		}
	}
	result.ImageFileIDs = nil
	for _, s := range slots {
		if s.FileID != "" {
			result.ImageFileIDs = append(result.ImageFileIDs, s.FileID)
		}
	}
	if len(result.ImageFileIDs) > 0 {
		result.ImageFileID = result.ImageFileIDs[len(result.ImageFileIDs)-1]
	}
}

// patchProxyConvID 修复 artifact URL 中 conv_id 为空（artifact 早于 conversation_id 回调时常见）。
func patchProxyConvID(rawURL, convID string) string {
	rawURL = strings.TrimSpace(rawURL)
	convID = strings.TrimSpace(convID)
	if rawURL == "" || convID == "" {
		return rawURL
	}
	if strings.Contains(rawURL, "conv_id=&") {
		return strings.Replace(rawURL, "conv_id=&", "conv_id="+convID+"&", 1)
	}
	if strings.Contains(rawURL, "conv_id=") {
		return rawURL
	}
	// 相对路径无 conv_id 时追加
	if strings.Contains(rawURL, "/api/image/proxy?") && strings.Contains(rawURL, "file_id=") {
		sep := "?"
		if strings.Contains(rawURL, "?") {
			sep = "&"
		}
		return rawURL + sep + "conv_id=" + convID
	}
	return rawURL
}
