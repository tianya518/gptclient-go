package server

import (
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

// TokenPool Token 池：持久化为 JSON；支持 AT、ST、RT 或并存，ST/RT 可自动续期 AT。
type TokenPool struct {
	mu            sync.Mutex
	entries       []storedToken
	errorKeys     map[string]bool
	roundIdx      int
	tokensFile    string
	refreshAhead  time.Duration
	oauthTokenURL string
	oauthClientID string
	stopRefresh   chan struct{}
}

// NewTokenPool 创建并从 JSON 文件加载 Token 池（兼容旧版行格式）。
func NewTokenPool(tokensFile string, refreshAhead time.Duration) *TokenPool {
	if refreshAhead <= 0 {
		refreshAhead = 5 * time.Minute
	}
	tp := &TokenPool{
		errorKeys:     make(map[string]bool),
		tokensFile:    tokensFile,
		refreshAhead:  refreshAhead,
		oauthTokenURL: defaultOAuthTokenURL,
		oauthClientID: defaultOAuthClientID,
	}
	tp.loadFromFile()
	return tp
}

// SetOAuthConfig 覆盖 refresh_token 换 AT 的端点与 client_id（留空则用默认）。
func (tp *TokenPool) SetOAuthConfig(oauthURL, clientID string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if oauthURL != "" {
		tp.oauthTokenURL = oauthURL
	}
	if clientID != "" {
		tp.oauthClientID = clientID
	}
}

func (tp *TokenPool) loadFromFile() {
	tokens := loadTokensFromFile(tp.tokensFile)
	migrated := false
	if len(tokens) == 0 && strings.HasSuffix(tp.tokensFile, ".json") {
		legacy := strings.TrimSuffix(tp.tokensFile, ".json") + ".txt"
		if legacy != tp.tokensFile {
			if legacyTokens := loadTokensFromFile(legacy); len(legacyTokens) > 0 {
				tokens = legacyTokens
				migrated = true
				log.Printf("[token-pool] 从旧文件 %s 迁移 %d 条凭证", legacy, len(tokens))
			}
		}
	}
	var atN, stN int
	for _, t := range tokens {
		if t.AccessToken != "" {
			atN++
		}
		if t.SessionToken != "" {
			stN++
		}
		tp.entries = append(tp.entries, t)
	}
	if len(tp.entries) > 0 {
		log.Printf("[token-pool] 已加载 %d 条凭证 (AT=%d, ST=%d) ← %s", len(tp.entries), atN, stN, tp.tokensFile)
		if migrated {
			_ = saveTokensToFile(tp.tokensFile, tp.entries)
		}
	}
}

func (tp *TokenPool) persistLocked() {
	if err := saveTokensToFile(tp.tokensFile, tp.entries); err != nil {
		log.Printf("[token-pool] 保存失败: %v", err)
	}
}

// fetchRefreshed 仅做网络换取，不修改池状态、不加锁；优先用 refresh_token，回退 session_token。
// 返回新的 access_token、（可能轮换的）refresh_token、过期时间。
func (tp *TokenPool) fetchRefreshed(e storedToken) (at, rt string, exp time.Time, err error) {
	if e.RefreshToken != "" {
		return RefreshATFromRefreshToken(e.RefreshToken, tp.oauthTokenURL, tp.oauthClientID)
	}
	if e.SessionToken != "" {
		at, exp, err = RefreshATFromSession(e.SessionToken)
		return at, "", exp, err
	}
	return "", "", time.Time{}, errors.New("no refreshable credential")
}

// canRefresh 判断该条目是否可通过 RT/ST 续期。
func (e storedToken) canRefresh() bool {
	return e.RefreshToken != "" || e.SessionToken != ""
}

func (tp *TokenPool) refreshEntry(e *storedToken) (string, error) {
	if !e.canRefresh() {
		if e.AccessToken == "" {
			return "", errors.New("no token")
		}
		return e.AccessToken, nil
	}
	at, rt, exp, err := tp.fetchRefreshed(*e)
	if err != nil {
		return "", err
	}
	via := "ST"
	if e.RefreshToken != "" {
		via = "RT"
	}
	e.AccessToken = at
	e.ExpiresAt = exp
	if rt != "" {
		e.RefreshToken = rt
	}
	e.UpdatedAt = time.Now()
	log.Printf("[token-pool] %s→AT 刷新成功, 过期时间 %s", via, exp.Format(time.RFC3339))
	tp.persistLocked()
	return at, nil
}

func (tp *TokenPool) ensureFresh(e *storedToken) (string, error) {
	if e.canRefresh() {
		need := e.AccessToken == "" || e.ExpiresAt.IsZero() || time.Now().Add(tp.refreshAhead).After(e.ExpiresAt)
		if need {
			return tp.refreshEntry(e)
		}
		return e.AccessToken, nil
	}
	if e.AccessToken == "" {
		return "", errors.New("empty access token")
	}
	if e.ExpiresAt.IsZero() {
		e.ExpiresAt = parseJWTExp(e.AccessToken)
	}
	// 纯 AT 条目无法续期：若已过期则报错，让 Pick 跳过并选下一条
	if !e.ExpiresAt.IsZero() && time.Now().After(e.ExpiresAt) {
		return "", errors.New("access token expired and no session/refresh token to renew")
	}
	return e.AccessToken, nil
}

// Pick 轮询选取可用 AT；含 ST 的条目会在过期前自动刷新。
func (tp *TokenPool) Pick() (string, bool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	n := len(tp.entries)
	if n == 0 {
		return "", false
	}

	for i := 0; i < n; i++ {
		idx := (tp.roundIdx + i) % n
		e := &tp.entries[idx]
		if tp.errorKeys[e.dedupKey()] {
			continue
		}
		at, err := tp.ensureFresh(e)
		if err != nil {
			log.Printf("[token-pool] 刷新失败 key=%s: %v", e.dedupKey(), err)
			tp.errorKeys[e.dedupKey()] = true
			continue
		}
		tp.roundIdx = (idx + 1) % n
		return at, true
	}
	return "", false
}

// TryRefreshAT 强制用 RT/ST 刷新与 currentAT 对应的条目（401 重试用）。
func (tp *TokenPool) TryRefreshAT(currentAT string) (string, bool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	for i := range tp.entries {
		e := &tp.entries[i]
		if !e.canRefresh() {
			continue
		}
		if e.AccessToken != "" && e.AccessToken != currentAT {
			continue
		}
		at, err := tp.refreshEntry(e)
		if err != nil {
			log.Printf("[token-pool] 强制刷新失败: %v", err)
			return "", false
		}
		delete(tp.errorKeys, e.dedupKey())
		return at, true
	}
	return "", false
}

// mergeToken 将新凭证合并进已有条目（同 ST 或同 AT 则更新）。
func mergeToken(existing *storedToken, incoming storedToken) {
	if incoming.AccessToken != "" {
		existing.AccessToken = incoming.AccessToken
		existing.ExpiresAt = incoming.ExpiresAt
		if existing.ExpiresAt.IsZero() {
			existing.ExpiresAt = parseJWTExp(incoming.AccessToken)
		}
	}
	if incoming.SessionToken != "" {
		existing.SessionToken = incoming.SessionToken
	}
	if incoming.RefreshToken != "" {
		existing.RefreshToken = incoming.RefreshToken
	}
	existing.UpdatedAt = time.Now()
	existing.assignID()
}

// Add 解析并添加凭证；整文件 JSON 重写保存（同时保留 access + session）。
func (tp *TokenPool) Add(chunks ...string) int {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	byKey := make(map[string]int, len(tp.entries))
	for i, e := range tp.entries {
		byKey[e.dedupKey()] = i
	}

	added := 0
	for _, raw := range chunks {
		incoming, ok := parseCredentialInput(raw)
		if !ok {
			continue
		}

		key := incoming.dedupKey()
		if key == "" {
			continue
		}

		// 同 session / refresh / access 则更新，避免重复条目
		merged := false
		for i := range tp.entries {
			e := &tp.entries[i]
			if incoming.SessionToken != "" && e.SessionToken == incoming.SessionToken {
				mergeToken(e, incoming)
				byKey[e.dedupKey()] = i
				merged = true
				break
			}
			if incoming.RefreshToken != "" && e.RefreshToken == incoming.RefreshToken {
				mergeToken(e, incoming)
				byKey[e.dedupKey()] = i
				merged = true
				break
			}
			if incoming.AccessToken != "" && e.AccessToken == incoming.AccessToken {
				mergeToken(e, incoming)
				byKey[e.dedupKey()] = i
				merged = true
				break
			}
		}
		if merged {
			added++
			delete(tp.errorKeys, key)
			continue
		}

		if _, dup := byKey[key]; dup {
			continue
		}

		tp.entries = append(tp.entries, incoming)
		byKey[key] = len(tp.entries) - 1
		added++
		delete(tp.errorKeys, key)
	}

	if added > 0 || len(tp.entries) > 0 {
		tp.persistLocked()
	}
	return added
}

// Clear 清空池与文件。
func (tp *TokenPool) Clear() {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.entries = nil
	tp.errorKeys = make(map[string]bool)
	tp.roundIdx = 0
	_ = saveTokensToFile(tp.tokensFile, nil)
}

// MarkError 标记失效（按当前 AT 匹配条目）。
func (tp *TokenPool) MarkError(at string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for i := range tp.entries {
		if tp.entries[i].AccessToken == at {
			tp.errorKeys[tp.entries[i].dedupKey()] = true
			return
		}
	}
	tp.errorKeys["at:"+at] = true
}

// Stats 返回 total / valid / errored。
func (tp *TokenPool) Stats() (total, valid, errored int) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	total = len(tp.entries)
	errored = len(tp.errorKeys)
	valid = total - errored
	if valid < 0 {
		valid = 0
	}
	return
}

// ErrorTokens 返回失效条目的 dedupKey 列表。
func (tp *TokenPool) ErrorTokens() []string {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	result := make([]string, 0, len(tp.errorKeys))
	for k := range tp.errorKeys {
		result = append(result, k)
	}
	return result
}

// StartRefreshLoop 启动后台定时刷新：每 interval 扫一遍池，对快过期的 RT/ST 条目提前续期。
// 启动后立即先跑一轮。重复调用会先停掉旧循环。
func (tp *TokenPool) StartRefreshLoop(interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	tp.StopRefreshLoop()
	stop := make(chan struct{})
	tp.mu.Lock()
	tp.stopRefresh = stop
	tp.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		tp.refreshDue()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				tp.refreshDue()
			}
		}
	}()
	log.Printf("[token-pool] 后台定时刷新已启动, 间隔 %s, 提前量 %s", interval, tp.refreshAhead)
}

// StopRefreshLoop 停止后台定时刷新循环。
func (tp *TokenPool) StopRefreshLoop() {
	tp.mu.Lock()
	stop := tp.stopRefresh
	tp.stopRefresh = nil
	tp.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// refreshDue 扫描并刷新快过期的条目：采用「快照 → 无锁换取 → 回写」，避免网络 IO 期间长期持锁。
func (tp *TokenPool) refreshDue() {
	type job struct {
		key string
		tok storedToken
	}
	now := time.Now()

	tp.mu.Lock()
	var jobs []job
	for _, e := range tp.entries {
		if !e.canRefresh() || tp.errorKeys[e.dedupKey()] {
			continue
		}
		need := e.AccessToken == "" || e.ExpiresAt.IsZero() || now.Add(tp.refreshAhead).After(e.ExpiresAt)
		if need {
			jobs = append(jobs, job{key: e.dedupKey(), tok: e})
		}
	}
	tp.mu.Unlock()

	if len(jobs) == 0 {
		return
	}

	ok, failed := 0, 0
	for _, j := range jobs {
		at, rt, exp, err := tp.fetchRefreshed(j.tok)

		tp.mu.Lock()
		for i := range tp.entries {
			if tp.entries[i].dedupKey() != j.key {
				continue
			}
			if err != nil {
				failed++
				log.Printf("[token-pool] 定时刷新失败 key=%s: %v", j.key, err)
			} else {
				tp.entries[i].AccessToken = at
				tp.entries[i].ExpiresAt = exp
				if rt != "" {
					tp.entries[i].RefreshToken = rt
				}
				tp.entries[i].UpdatedAt = time.Now()
				delete(tp.errorKeys, j.key)
				ok++
			}
			break
		}
		tp.mu.Unlock()
	}

	if ok > 0 {
		tp.mu.Lock()
		tp.persistLocked()
		tp.mu.Unlock()
	}
	log.Printf("[token-pool] 定时刷新完成: 成功 %d, 失败 %d, 待刷新 %d", ok, failed, len(jobs))
}
