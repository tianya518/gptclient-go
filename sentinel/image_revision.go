package sentinel

// image_revision.go —— 「图位」（slot）管理与生图修订下发。
//
// 生图可能多图（batch_requests=N）且同一图位会被多次修订（如改图 edit_op），本文件负责：
//   - 把解析出的生图条目归并到图位（assignImageSlot，按 gen_id / message_id / 父图关系）；
//   - 按 artifact_image_revisions 策略（all / latest_per_slot / final_only）向客户端下发产物事件；
//   - 定稿（FinalizeImageGenSlots）与最终 file_id 列表重建（RebuildImageFileIDsFromSlots）。
// 收图轮询见 image_poll.go；消息解析见 image_parse.go；状态判定见 image_state.go。

import (
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
	SlotIndex   int
	GenID       string
	MessageID   string
	FileID      string
	Revision    int
	FileHistory []string
	Final       bool
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

func (c *Client) noteGeneratedImageRevision(result *ChatResult, opts ChatOptions, p ParsedGeneratedImage, wsUpdateType string) {
	if p.FileID == "" || result == nil || !result.ExpectGeneratedImages {
		return
	}
	// 经典 DALL·E 带 gen_id；picture_v2/image_gen 工具常无 gen_id，但仍为有效产出
	// conv_poll 路径：从 conversation API 拿到的图片节点，直接接受（已在 tryNoteGeneratedImagesFromMessage 做过过滤）
	if p.GenID == "" && wsUpdateType != "finalize" && wsUpdateType != "conv_poll" {
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
				Event:      StreamEventArtifactSuperseded,
				Kind:       "generated_image",
				SlotIndex:  slot.SlotIndex,
				Revision:   slot.Revision - 1,
				GenID:      slot.GenID,
				MessageID:  slot.MessageID,
				FileID:     prevFileID,
				UpdateType: wsUpdateType,
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
				Event:     StreamEventArtifactSlotFinal,
				Kind:      "generated_image",
				SlotIndex: slot.SlotIndex,
				Revision:  slot.Revision,
				GenID:     slot.GenID,
				MessageID: slot.MessageID,
				FileID:    slot.FileID,
				IsFinal:   true,
				Total:     len(result.imageSlots),
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
