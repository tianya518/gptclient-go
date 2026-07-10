package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sessionAuthURL = "https://chatgpt.com/api/auth/session"

// OAuth refresh_token 换 access_token 的默认端点与 client_id（与 pandora/chatgpt2api 一致）。
const (
	defaultOAuthTokenURL = "https://auth.openai.com/oauth/token"
	defaultOAuthClientID = "app_2SKx67EdpoN0G6j64rFvigXD"
)

var sessionHTTPClient = &http.Client{Timeout: 30 * time.Second}
var oauthHTTPClient = &http.Client{Timeout: 60 * time.Second}

// RefreshATFromSession 用 Session Token 换取新的 Access Token（Web 作用域）。
func RefreshATFromSession(sessionToken string) (accessToken string, expiresAt time.Time, err error) {
	sessionToken = normalizeSessionToken(sessionToken)
	if sessionToken == "" {
		return "", time.Time{}, errors.New("session token is empty")
	}

	req, err := http.NewRequest(http.MethodGet, sessionAuthURL, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.AddCookie(&http.Cookie{Name: "__Secure-next-auth.session-token", Value: sessionToken})

	resp, err := sessionHTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("session refresh request: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("session refresh http=%d body=%s", resp.StatusCode, truncateBody(string(data), 200))
	}

	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "{}" {
		return "", time.Time{}, errors.New("session token expired or invalid (empty response)")
	}

	var out struct {
		AccessToken string `json:"accessToken"`
		Expires     string `json:"expires"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", time.Time{}, fmt.Errorf("parse session response: %w", err)
	}
	if out.AccessToken == "" {
		return "", time.Time{}, errors.New("session response missing accessToken")
	}

	expiresAt = time.Time{}
	if out.Expires != "" {
		if t, e := time.Parse(time.RFC3339, out.Expires); e == nil {
			expiresAt = t
		}
	}
	if expiresAt.IsZero() {
		expiresAt = parseJWTExp(out.AccessToken)
	}
	return out.AccessToken, expiresAt, nil
}

// RefreshATFromRefreshToken 用 OAuth refresh_token 换取新的 Access Token。
// 返回新的 access_token、（可能轮换的）refresh_token 与过期时间。
// oauthURL / clientID 为空时使用默认值。
func RefreshATFromRefreshToken(refreshToken, oauthURL, clientID string) (accessToken, newRefreshToken string, expiresAt time.Time, err error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return "", "", time.Time{}, errors.New("refresh token is empty")
	}
	if oauthURL == "" {
		oauthURL = defaultOAuthTokenURL
	}
	if clientID == "" {
		clientID = defaultOAuthClientID
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", clientID)

	req, err := http.NewRequest(http.MethodPost, oauthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("oauth refresh request: %w", err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", time.Time{}, fmt.Errorf("oauth refresh http=%d body=%s", resp.StatusCode, truncateBody(string(data), 200))
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", "", time.Time{}, fmt.Errorf("parse oauth response: %w", err)
	}
	if out.AccessToken == "" {
		return "", "", time.Time{}, errors.New("oauth response missing access_token")
	}
	return out.AccessToken, strings.TrimSpace(out.RefreshToken), parseJWTExp(out.AccessToken), nil
}

// normalizeSessionToken 去掉常见前缀，得到裸 Session Token。
func normalizeSessionToken(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "st:")
	s = strings.TrimPrefix(s, "session:")
	s = strings.TrimPrefix(s, "Bearer ")
	s = strings.TrimPrefix(s, "bearer ")
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "__secure-next-auth.session-token") {
		if i := strings.Index(s, "="); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		}
	}
	return strings.TrimSpace(s)
}

// parseJWTExp 从 JWT payload 解析 exp；失败则默认 +24h。
func parseJWTExp(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Now().Add(24 * time.Hour)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Now().Add(24 * time.Hour)
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp == 0 {
		return time.Now().Add(24 * time.Hour)
	}
	return time.Unix(claims.Exp, 0)
}

func isAccessToken(s string) bool {
	if !strings.HasPrefix(s, "eyJ") || strings.Count(s, ".") < 2 {
		return false
	}
	// JWE Session Token（alg=dir）不是 Access Token
	if strings.HasPrefix(s, "eyJhbGciOiJkaXIi") {
		return false
	}
	return true
}

func isLikelySessionToken(s string) bool {
	if isAccessToken(s) || strings.Contains(s, "{") || strings.Contains(s, " ") {
		return false
	}
	return len(s) >= 40
}

func truncateBody(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
