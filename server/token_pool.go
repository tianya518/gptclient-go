package server

import (
	"bufio"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
)

var accessTokenRegex = regexp.MustCompile(`"accessToken"\s*:\s*"([^"]+)"`)

// cleanToken 提取可能包含的 accessToken 字段，如果提取不到则判断原字符串是否可能是合法 token
func cleanToken(t string) string {
	t = strings.TrimSpace(t)
	if match := accessTokenRegex.FindStringSubmatch(t); len(match) == 2 {
		return match[1]
	}

	// 如果包含大括号，说明可能是没匹配到正则的 JSON 碎片，或者是无效数据
	if strings.Contains(t, "{") || strings.Contains(t, "}") {
		return ""
	}
	return t
}

// TokenPool Token 池，支持多账号轮询
// 对应 chat2api 的 utils/globals.py
type TokenPool struct {
	mu          sync.RWMutex
	tokens      []string        // 所有 token
	errorTokens map[string]bool // 失效 token 集合
	roundIdx    int             // 轮询游标
	tokensFile  string          // 持久化文件路径
}

// NewTokenPool 创建并从文件加载 Token 池
func NewTokenPool(tokensFile string) *TokenPool {
	tp := &TokenPool{
		errorTokens: make(map[string]bool),
		tokensFile:  tokensFile,
	}
	tp.loadFromFile()
	return tp
}

// loadFromFile 从文件加载 tokens（每行一个，# 开头为注释）
func (tp *TokenPool) loadFromFile() {
	f, err := os.Open(tp.tokensFile)
	if err != nil {
		// 文件不存在时静默忽略（首次启动）
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = cleanToken(line)
		if line == "" {
			log.Printf("[token-pool] 跳过无效行（无法提取 accessToken）")
			continue
		}
		tp.tokens = append(tp.tokens, line)
	}
	log.Printf("[token-pool] 已加载 %d 个 Token", len(tp.tokens))
}

// Pick 从池中选取下一个可用 Token（轮询，跳过失效 token）
// 返回 ("", false) 表示池中无可用 Token
func (tp *TokenPool) Pick() (string, bool) {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	n := len(tp.tokens)
	if n == 0 {
		return "", false
	}

	// 从当前游标开始轮询，找到第一个不在 errorTokens 中的 token
	for i := 0; i < n; i++ {
		idx := (tp.roundIdx + i) % n
		t := tp.tokens[idx]
		if !tp.errorTokens[t] {
			tp.roundIdx = (idx + 1) % n
			return t, true
		}
	}
	return "", false
}

// Add 添加 Token（持久化到文件）
func (tp *TokenPool) Add(tokens ...string) int {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	added := 0
	f, _ := os.OpenFile(tp.tokensFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer func() {
		if f != nil {
			f.Close()
		}
	}()

	existing := make(map[string]bool, len(tp.tokens))
	for _, t := range tp.tokens {
		existing[t] = true
	}

	for _, t := range tokens {
		t = cleanToken(t)
		if t == "" || strings.HasPrefix(t, "#") || existing[t] {
			continue
		}
		tp.tokens = append(tp.tokens, t)
		existing[t] = true
		added++
		if f != nil {
			_, _ = f.WriteString(t + "\n")
		}
	}
	return added
}

// Clear 清空所有 Token
func (tp *TokenPool) Clear() {
	tp.mu.Lock()
	defer tp.mu.Unlock()

	tp.tokens = nil
	tp.errorTokens = make(map[string]bool)
	tp.roundIdx = 0

	// 清空文件
	_ = os.WriteFile(tp.tokensFile, []byte{}, 0644)
}

// MarkError 将 Token 标记为失效
func (tp *TokenPool) MarkError(token string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	tp.errorTokens[token] = true
}

// Stats 返回当前状态
func (tp *TokenPool) Stats() (total, valid, errored int) {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	total = len(tp.tokens)
	errored = len(tp.errorTokens)
	valid = total - errored
	if valid < 0 {
		valid = 0
	}
	return
}

// ErrorTokens 返回失效 Token 列表
func (tp *TokenPool) ErrorTokens() []string {
	tp.mu.RLock()
	defer tp.mu.RUnlock()
	result := make([]string, 0, len(tp.errorTokens))
	for t := range tp.errorTokens {
		result = append(result, t)
	}
	return result
}
