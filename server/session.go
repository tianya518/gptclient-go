package server

import (
	"log"
	"sync"
	"time"

	sentinel "sentinel-go"
)

// sessionEntry 单个会话条目
type sessionEntry struct {
	client   *sentinel.Client
	lastUsed time.Time
	token    string // 该 session 绑定的 ChatGPT token
}

// SessionManager 有状态多轮对话管理器
// key = conversationID（来自 ChatGPT 服务端，首轮对话后写入）
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*sessionEntry
	ttl      time.Duration
	cfg      *ServerConfig
}

// NewSessionManager 创建 Session 管理器
func NewSessionManager(cfg *ServerConfig) *SessionManager {
	sm := &SessionManager{
		sessions: make(map[string]*sessionEntry),
		ttl:      time.Duration(cfg.SessionTTLMinutes) * time.Minute,
		cfg:      cfg,
	}
	go sm.cleanupLoop()
	return sm
}

// GetSession 获取指定的 session
func (sm *SessionManager) GetSession(convID string) (*sessionEntry, bool) {
	if convID == "" {
		return nil, false
	}
	sm.mu.RLock()
	entry, ok := sm.sessions[convID]
	sm.mu.RUnlock()
	return entry, ok
}

// GetOrCreate 根据 conversationID 获取已有 session 或创建新 session
//   - convID == ""：创建新 Client（新对话），返回 entry
//   - convID != ""：查找已有 session，若不存在则新建（防止 session 过期后重建）
func (sm *SessionManager) GetOrCreate(convID, token string) *sessionEntry {
	if convID != "" {
		sm.mu.RLock()
		entry, ok := sm.sessions[convID]
		sm.mu.RUnlock()
		if ok {
			sm.mu.Lock()
			entry.lastUsed = time.Now()
			sm.mu.Unlock()
			return entry
		}
	}

	// 新建 Client
	client := sentinel.NewClient(sentinel.Config{
		BearerToken: token,
		Model:       sm.cfg.DefaultModel,
		TempMode:    sm.cfg.TempMode,
		ImageDir:    sm.cfg.ImageDir,
	})
	// 启用自动图片下载阻塞，确保 Web UI 能够获取并渲染图片
	client.SetDisableAutoImage(false)

	entry := &sessionEntry{
		client:   client,
		lastUsed: time.Now(),
		token:    token,
	}
	// 注意：此时 convID 可能为空，新对话的 conversationID 要等第一轮结束后才知道
	// 见 handler_chat.go 中在对话完成后调用 sm.Register()
	return entry
}

// Register 对话完成后，将 entry 注册到 conversationID 下
func (sm *SessionManager) Register(convID string, entry *sessionEntry) {
	if convID == "" {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	entry.lastUsed = time.Now()
	sm.sessions[convID] = entry
}

// Delete 主动删除一个 session
func (sm *SessionManager) Delete(convID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.sessions, convID)
}

// Count 返回当前活跃 session 数
func (sm *SessionManager) Count() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.sessions)
}

// cleanupLoop 后台定期清理过期 session
func (sm *SessionManager) cleanupLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		sm.cleanup()
	}
}

func (sm *SessionManager) cleanup() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	now := time.Now()
	removed := 0
	for convID, entry := range sm.sessions {
		if now.Sub(entry.lastUsed) > sm.ttl {
			delete(sm.sessions, convID)
			removed++
		}
	}
	if removed > 0 {
		log.Printf("[session] 清理过期 session %d 个，当前活跃 %d 个", removed, len(sm.sessions))
	}
}
