package sentinel

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// StreamRecorder 记录 SSE / WebSocket 原始事件与解析出的产物信号（用于 stream-capture 分析）。
type StreamRecorder struct {
	mu      sync.Mutex
	entries []StreamRecord
	file    *os.File
	wsFile  *os.File
	wsSeq   int
}

// StreamRecord 单条 SSE 记录（NDJSON 一行）。
type StreamRecord struct {
	Seq       int              `json:"seq"`
	At        string           `json:"at"`
	SSEEvent  string           `json:"sse_event,omitempty"`
	Data      string           `json:"data,omitempty"`
	EventType string           `json:"event_type,omitempty"`
	Signals   []ArtifactSignal `json:"signals,omitempty"`
}

// WSRecord 单条 WebSocket 帧记录（NDJSON 一行）。
type WSRecord struct {
	Seq       int    `json:"seq"`
	At        string `json:"at"`
	FrameType string `json:"frame_type,omitempty"`
	Len       int    `json:"len"`
	HasImage  bool   `json:"has_image_ref"`
	Snippet   string `json:"snippet,omitempty"`
}

// NewStreamRecorder 创建记录器；outPath 非空时同步追加写入 sse.ndjson。
func NewStreamRecorder(outPath string) (*StreamRecorder, error) {
	r := &StreamRecorder{}
	if outPath == "" {
		return r, nil
	}
	f, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	r.file = f
	return r, nil
}

// OpenWSLog 追加记录 WebSocket 帧到 ws.ndjson。
func (r *StreamRecorder) OpenWSLog(wsPath string) error {
	if r == nil || wsPath == "" {
		return nil
	}
	f, err := os.Create(wsPath)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.wsFile = f
	r.mu.Unlock()
	return nil
}

// RecordSSE 记录一条 SSE data 事件。
func (r *StreamRecorder) RecordSSE(sseEvent, data string, parsed map[string]interface{}) {
	if r == nil {
		return
	}
	var signals []ArtifactSignal
	evtType := ""
	if parsed != nil {
		signals = ExtractSignalsFromJSON(parsed)
		evtType, _ = parsed["type"].(string)
	}
	rec := StreamRecord{
		At:        time.Now().Format(time.RFC3339Nano),
		SSEEvent:  sseEvent,
		Data:      data,
		EventType: evtType,
		Signals:   signals,
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec.Seq = len(r.entries) + 1
	r.entries = append(r.entries, rec)
	if r.file != nil {
		b, _ := json.Marshal(rec)
		_, _ = r.file.Write(append(b, '\n'))
	}
}

// RecordWS 记录一条 WebSocket 原始帧（截断预览）。
func (r *StreamRecorder) RecordWS(frameType string, raw []byte) {
	if r == nil || r.wsFile == nil {
		return
	}
	s := string(raw)
	rec := WSRecord{
		At:        time.Now().Format(time.RFC3339Nano),
		FrameType: frameType,
		Len:       len(raw),
		HasImage:  strings.Contains(s, "sediment://") || strings.Contains(s, "image_asset_pointer"),
		Snippet:   truncateStr(s, 800),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wsSeq++
	rec.Seq = r.wsSeq
	b, _ := json.Marshal(rec)
	_, _ = r.wsFile.Write(append(b, '\n'))
}

// Entries 返回全部记录副本。
func (r *StreamRecorder) Entries() []StreamRecord {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]StreamRecord, len(r.entries))
	copy(out, r.entries)
	return out
}

// AllSignals 合并全部记录中的信号。
func (r *StreamRecorder) AllSignals() []ArtifactSignal {
	entries := r.Entries()
	var all []ArtifactSignal
	for _, e := range entries {
		all = MergeSignals(all, e.Signals)
	}
	return all
}

// Close 关闭底层文件。
func (r *StreamRecorder) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	if r.file != nil {
		err = r.file.Close()
		r.file = nil
	}
	if r.wsFile != nil {
		if e := r.wsFile.Close(); e != nil && err == nil {
			err = e
		}
		r.wsFile = nil
	}
	return err
}
