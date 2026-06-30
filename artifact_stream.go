package sentinel

import (
	"encoding/base64"
	"fmt"
	"path"
	"strings"
)

func artifactKeyImage(fileID string) string { return "img:" + fileID }
func artifactKeySandbox(msgID, sandboxPath string) string {
	return "sandbox:" + msgID + ":" + sandboxPath
}

func guessMimeFromName(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".zip":
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

func (c *Client) emitImageGenPending(cfg ArtifactStreamConfig, title string) {
	if strings.TrimSpace(title) == "" {
		title = "正在生成图片…"
	}
	cfg.emit(StreamEvent{
		Event: StreamEventArtifactPending,
		Kind:  "generated_image",
		Title: title,
	})
}

func (c *Client) emitGeneratedImage(cfg ArtifactStreamConfig, result *ChatResult, fileID string) {
	if fileID == "" || result == nil {
		return
	}
	key := artifactKeyImage(fileID)
	if result.emittedArtifacts == nil {
		result.emittedArtifacts = make(map[string]bool)
	}
	if result.emittedArtifacts[key] {
		return
	}
	result.emittedArtifacts[key] = true

	cfg = cfg.normalized()
	idx := len(result.emittedArtifacts) // approximate; use ImageFileIDs index
	for i, id := range result.ImageFileIDs {
		if id == fileID {
			idx = i + 1
			break
		}
	}
	if idx == 0 {
		idx = len(result.ImageFileIDs)
		if idx == 0 {
			idx = 1
		}
	}

	evBase := StreamEvent{
		Event:    StreamEventArtifact,
		Kind:     "generated_image",
		Index:    idx,
		FileID:   fileID,
		MimeType: "image/png",
		Name:     fmt.Sprintf("generated_%d.png", idx),
	}
	if cfg.BuildImageURL != nil {
		evBase.URL = cfg.BuildImageURL(fileID)
	}

	switch cfg.Delivery {
	case ArtifactDeliveryURL:
		cfg.emit(evBase)
		return
	}

	data, mime, err := c.DownloadFileByFileID(result.ConversationID, fileID)
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

func (c *Client) emitSandboxFile(cfg ArtifactStreamConfig, result *ChatResult, art SandboxArtifact) {
	if art.SandboxPath == "" {
		return
	}
	msgID := art.MessageID
	if msgID == "" {
		msgID = result.LastAssistantMsgID
	}
	key := artifactKeySandbox(msgID, art.SandboxPath)
	if result.emittedArtifacts == nil {
		result.emittedArtifacts = make(map[string]bool)
	}
	if result.emittedArtifacts[key] {
		return
	}
	result.emittedArtifacts[key] = true

	cfg = cfg.normalized()
	name := art.FileName
	if name == "" {
		name = path.Base(art.SandboxPath)
	}
	idx := 0
	for i, a := range result.SandboxArtifacts {
		if a.SandboxPath == art.SandboxPath && a.MessageID == art.MessageID {
			idx = i + 1
			break
		}
	}
	if idx == 0 {
		idx = len(result.SandboxArtifacts)
		if idx == 0 {
			idx = 1
		}
	}

	evBase := StreamEvent{
		Event:       StreamEventArtifact,
		Kind:        "sandbox_file",
		Index:       idx,
		Name:        name,
		MimeType:    guessMimeFromName(name),
		MessageID:   msgID,
		SandboxPath: art.SandboxPath,
	}
	if cfg.BuildSandboxURL != nil {
		evBase.URL = cfg.BuildSandboxURL(msgID, art.SandboxPath)
	}

	switch cfg.Delivery {
	case ArtifactDeliveryURL:
		cfg.emit(evBase)
		return
	}

	data, mime, err := c.DownloadSandboxFile(result.ConversationID, msgID, art.SandboxPath)
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

func (c *Client) emitArtifactBytes(cfg ArtifactStreamConfig, meta StreamEvent, data []byte) {
	if len(data) == 0 {
		cfg.emit(meta)
		cfg.emit(StreamEvent{
			Event:      StreamEventArtifactDone,
			Kind:       meta.Kind,
			Index:      meta.Index,
			FileID:     meta.FileID,
			MessageID:  meta.MessageID,
			SandboxPath: meta.SandboxPath,
			SizeBytes:  0,
		})
		return
	}

	if cfg.Delivery == ArtifactDeliveryBase64 {
		meta.Data = base64.StdEncoding.EncodeToString(data)
		meta.SizeBytes = len(data)
		cfg.emit(meta)
		cfg.emit(StreamEvent{
			Event:       StreamEventArtifactDone,
			Kind:        meta.Kind,
			Index:       meta.Index,
			FileID:      meta.FileID,
			MessageID:   meta.MessageID,
			SandboxPath: meta.SandboxPath,
			SizeBytes:   len(data),
		})
		return
	}

	chunkSize := cfg.ChunkSize
	total := (len(data) + chunkSize - 1) / chunkSize
	for i := 0; i < total; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		chunk := data[start:end]
		cfg.emit(StreamEvent{
			Event:       StreamEventArtifactChunk,
			Kind:        meta.Kind,
			Index:       meta.Index,
			FileID:      meta.FileID,
			MessageID:   meta.MessageID,
			SandboxPath: meta.SandboxPath,
			Name:        meta.Name,
			MimeType:    meta.MimeType,
			ChunkIndex:  i + 1,
			ChunkTotal:  total,
			Data:        base64.StdEncoding.EncodeToString(chunk),
			SizeBytes:   len(chunk),
		})
	}
	cfg.emit(StreamEvent{
		Event:       StreamEventArtifactDone,
		Kind:        meta.Kind,
		Index:       meta.Index,
		FileID:      meta.FileID,
		MessageID:   meta.MessageID,
		SandboxPath: meta.SandboxPath,
		SizeBytes:   len(data),
	})
}

// EmitNewArtifacts 将本轮新出现的沙箱产物推送给客户端（生图由 noteGeneratedImageRevision 按槽位推送）。
func (c *Client) EmitNewArtifacts(cfg ArtifactStreamConfig, result *ChatResult) {
	if cfg.OnEvent == nil || result == nil {
		return
	}
	for _, art := range result.SandboxArtifacts {
		c.emitSandboxFile(cfg, result, art)
	}
}
