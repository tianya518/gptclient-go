package server

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
)

const tokenFileVersion = 1

var (
	accessTokenRegex  = regexp.MustCompile(`"accessToken"\s*:\s*"([^"]+)"`)
	sessionTokenRegex = regexp.MustCompile(`"sessionToken"\s*:\s*"([^"]+)"`)
	refreshTokenRegex = regexp.MustCompile(`"refresh_token"\s*:\s*"([^"]+)"`)
)

// storedToken 是 tokens 文件中的一条凭证（Access + Session + Refresh）。
type storedToken struct {
	ID           string    `json:"id,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	SessionToken string    `json:"session_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"` // OAuth refresh token（auth.openai.com/oauth/token 换 AT）
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitempty"`
}

type tokenFile struct {
	Version int           `json:"version"`
	Tokens  []storedToken `json:"tokens"`
}

func (t *storedToken) dedupKey() string {
	if t.SessionToken != "" {
		return "st:" + t.SessionToken
	}
	if t.RefreshToken != "" {
		return "rt:" + t.RefreshToken
	}
	if t.AccessToken != "" {
		return "at:" + t.AccessToken
	}
	return ""
}

func (t *storedToken) assignID() {
	if t.ID != "" {
		return
	}
	src := t.SessionToken
	if src == "" {
		src = t.RefreshToken
	}
	if src == "" {
		src = t.AccessToken
	}
	if src == "" {
		return
	}
	sum := sha256.Sum256([]byte(src))
	t.ID = fmt.Sprintf("%x", sum[:6])
}

func newStoredToken(accessToken, sessionToken, refreshToken string) storedToken {
	t := storedToken{
		AccessToken:  accessToken,
		SessionToken: sessionToken,
		RefreshToken: refreshToken,
		UpdatedAt:    time.Now(),
	}
	if accessToken != "" {
		t.ExpiresAt = parseJWTExp(accessToken)
	}
	t.assignID()
	return t
}

// parseCredentialInput 解析单条导入：Access / Session / Access----Session / session JSON。
func parseCredentialInput(raw string) (storedToken, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.HasPrefix(raw, "#") {
		return storedToken{}, false
	}

	// chatgpt.com/api/auth/session 整段 JSON，或含 refresh_token 的账号 JSON（可含换行）
	if strings.Contains(raw, "{") {
		compact := strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "")
		compact = strings.TrimSpace(compact)
		at, st, rt := extractSessionJSON(compact)
		if at == "" && st == "" && rt == "" {
			at, st, rt = extractSessionJSON(raw)
		}
		if at != "" || st != "" || rt != "" {
			return newStoredToken(at, st, rt), true
		}
	}

	lower := strings.ToLower(raw)

	// Access----Session
	if idx := strings.Index(raw, "----"); idx >= 0 {
		at := strings.TrimSpace(raw[:idx])
		st := normalizeSessionToken(raw[idx+4:])
		if isAccessToken(at) || isSessionToken(st) {
			return newStoredToken(at, st, ""), true
		}
		return storedToken{}, false
	}

	// rt:<refresh_token> 显式声明 refresh token
	if strings.HasPrefix(lower, "rt:") || strings.HasPrefix(lower, "refresh:") {
		if i := strings.Index(raw, ":"); i >= 0 {
			rt := strings.TrimSpace(raw[i+1:])
			if rt != "" {
				return newStoredToken("", "", rt), true
			}
		}
		return storedToken{}, false
	}

	if strings.HasPrefix(lower, "st:") {
		st := normalizeSessionToken(raw[3:])
		if isSessionToken(st) {
			return newStoredToken("", st, ""), true
		}
		return storedToken{}, false
	}
	if strings.HasPrefix(lower, "session:") {
		if i := strings.Index(raw, ":"); i >= 0 {
			st := normalizeSessionToken(raw[i+1:])
			if isSessionToken(st) {
				return newStoredToken("", st, ""), true
			}
		}
		return storedToken{}, false
	}

	if isAccessToken(raw) {
		return newStoredToken(raw, "", ""), true
	}
	if isSessionToken(raw) {
		return newStoredToken("", normalizeSessionToken(raw), ""), true
	}

	return storedToken{}, false
}

// splitUploadText 将粘贴内容拆成多条待解析凭证。
func splitUploadText(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.Contains(raw, "{") && (strings.Contains(raw, "accessToken") || strings.Contains(raw, "refresh_token")) {
		joined := strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "")
		joined = strings.TrimSpace(joined)
		if at, st, rt := extractSessionJSON(joined); at != "" || st != "" || rt != "" {
			return []string{joined}
		}
		if at, st, rt := extractSessionJSON(raw); at != "" || st != "" || rt != "" {
			return []string{raw}
		}
	}
	var lines []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

func extractSessionJSON(line string) (at, st, rt string) {
	if !strings.Contains(line, "{") {
		return "", "", ""
	}
	if m := accessTokenRegex.FindStringSubmatch(line); len(m) == 2 && isAccessToken(m[1]) {
		at = m[1]
	}
	if m := sessionTokenRegex.FindStringSubmatch(line); len(m) == 2 {
		st = normalizeSessionToken(m[1])
	}
	if m := refreshTokenRegex.FindStringSubmatch(line); len(m) == 2 {
		rt = strings.TrimSpace(m[1])
	}
	return at, st, rt
}

func isSessionToken(s string) bool {
	s = normalizeSessionToken(s)
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "eyJhbGciOiJkaXIi") {
		return true
	}
	return isLikelySessionToken(s)
}

func loadTokensFromFile(path string) []storedToken {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}

	var tf tokenFile
	if err := json.Unmarshal(data, &tf); err == nil && tf.Version > 0 {
		return tf.Tokens
	}

	// 兼容旧版按行存储（st: / JWT / JSON 行）
	var out []storedToken
	for _, line := range strings.Split(string(data), "\n") {
		if t, ok := parseCredentialInput(line); ok {
			out = append(out, t)
		}
	}
	if len(out) > 0 {
		log.Printf("[token-store] 已从旧格式迁移 %d 条凭证 → 建议保存为 JSON", len(out))
	}
	return out
}

func saveTokensToFile(path string, tokens []storedToken) error {
	tf := tokenFile{Version: tokenFileVersion, Tokens: tokens}
	for i := range tf.Tokens {
		tf.Tokens[i].assignID()
	}
	b, err := json.MarshalIndent(tf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}

// cleanToken 从 JSON 提取 accessToken，或校验裸 JWT；无效则返回空串以回退 Token 池。
func cleanToken(t string) string {
	t = strings.TrimSpace(t)
	if match := accessTokenRegex.FindStringSubmatch(t); len(match) == 2 && isAccessToken(match[1]) {
		return match[1]
	}
	if idx := strings.Index(t, "----"); idx >= 0 {
		at := strings.TrimSpace(t[:idx])
		if isAccessToken(at) {
			return at
		}
	}
	if strings.Contains(t, "{") || strings.Contains(t, "}") {
		return ""
	}
	if isAccessToken(t) {
		return t
	}
	return ""
}
