package sentinel

// ArtifactDelivery 产物下发方式（请求 artifact_delivery）。
const (
	ArtifactDeliveryURL          = "url"
	ArtifactDeliveryBase64       = "base64"
	ArtifactDeliveryBase64Chunked = "base64_chunked"
)

// Stream 事件类型（写入 SSE chunk 的 sentinel 字段）。
const (
	StreamEventArtifactPending = "artifact_pending"
	StreamEventArtifact        = "artifact"
	StreamEventArtifactChunk   = "artifact_chunk"
	StreamEventArtifactDone       = "artifact_done"
	StreamEventArtifactSuperseded = "artifact_superseded" // 同槽位被新版本替换
	StreamEventArtifactSlotFinal  = "artifact_slot_final" // 该图位已定稿（多图 idle 结束）
)

// StreamEvent 流式侧信道：正文走 delta.content，产物/进度走 sentinel。
type StreamEvent struct {
	Event string `json:"event"`

	// generated_image | sandbox_file
	Kind string `json:"kind,omitempty"`

	// artifact_pending
	Title string `json:"title,omitempty"`

	// 多产物序号（从 1 开始）
	Index int `json:"index,omitempty"`
	Total int `json:"total,omitempty"`

	// 生图多版本：图1/图2 槽位与槽内修订次数
	SlotIndex   int    `json:"slot_index,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	GenID       string `json:"gen_id,omitempty"`
	ParentGenID string `json:"parent_gen_id,omitempty"`
	UpdateType  string `json:"update_type,omitempty"` // async-task-update-message 等
	IsFinal     bool   `json:"is_final,omitempty"`
	SupersedesFileID string `json:"supersedes_file_id,omitempty"`

	Name       string `json:"name,omitempty"`
	MimeType   string `json:"mime_type,omitempty"`
	SizeBytes  int    `json:"size_bytes,omitempty"`
	URL        string `json:"url,omitempty"`
	FileID     string `json:"file_id,omitempty"`
	MessageID  string `json:"message_id,omitempty"`
	SandboxPath string `json:"sandbox_path,omitempty"`

	// base64 或 base64_chunked
	Data       string `json:"data,omitempty"`
	ChunkIndex int    `json:"chunk_index,omitempty"`
	ChunkTotal int    `json:"chunk_total,omitempty"`

	Error string `json:"error,omitempty"`
}

// ArtifactStreamConfig 产物如何流式交给客户端。
type ArtifactStreamConfig struct {
	Delivery  string // url | base64 | base64_chunked
	ChunkSize int    // base64_chunked 分块大小（原始字节），默认 384KiB
	// ImageRevisions: all | latest_per_slot（默认）| final_only
	ImageRevisions string
	OnEvent        func(StreamEvent)
	// BuildImageURL(fileID) / BuildSandboxURL(msgID, path) 由 server 注入
	BuildImageURL   func(fileID string) string
	BuildSandboxURL func(messageID, sandboxPath string) string
}

func (cfg *ArtifactStreamConfig) normalized() ArtifactStreamConfig {
	out := *cfg
	if out.Delivery == "" {
		out.Delivery = ArtifactDeliveryURL
	}
	if out.ChunkSize <= 0 {
		out.ChunkSize = 384 * 1024
	}
	return out
}

func (cfg ArtifactStreamConfig) emit(ev StreamEvent) {
	if cfg.OnEvent != nil {
		cfg.OnEvent(ev)
	}
}
