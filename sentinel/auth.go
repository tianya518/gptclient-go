package sentinel

import (
	"encoding/json"
	"fmt"
	"time"
)

// getConduitToken 获取 conduit_token（Step 1）
func (c *Client) getConduitToken(model, turnTraceID, partialText string) (string, error) {
	if partialText == "" {
		partialText = "h"
	}

	body := map[string]interface{}{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":    "client-created-root",
		"model":                model,
		"timezone_offset_min":  -480,
		"timezone":             "Asia/Shanghai",
		"conversation_mode":    map[string]string{"kind": "primary_assistant"},
		"system_hints":         []string{},
		"partial_query": map[string]interface{}{
			"id":     GenerateUUID(),
			"author": map[string]string{"role": "user"},
			"content": map[string]interface{}{
				"content_type": "text",
				"parts":        []string{partialText},
			},
		},
		"supports_buffering":    true,
		"supported_encodings":   []string{"v1"},
		"client_contextual_info": map[string]interface{}{"app_name": "chatgpt.com"},
		"thinking_effort":       "standard",
	}

	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                 "*/*",
			"Content-Type":           "application/json",
			"x-conduit-token":        "no-token",
			"x-oai-turn-trace-id":    turnTraceID,
			"x-openai-target-path":   "/backend-api/f/conversation/prepare",
			"x-openai-target-route":  "/backend-api/f/conversation/prepare",
		}).
		SetBody(body).
		Post("/backend-api/f/conversation/prepare")
	if err != nil {
		return "", fmt.Errorf("conversation/prepare request: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("conversation/prepare %d: %s", resp.StatusCode, truncateStr(resp.String(), 200))
	}

	var result struct {
		Status       string `json:"status"`
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.Unmarshal(resp.Bytes(), &result); err != nil {
		return "", fmt.Errorf("parse conduit response: %w", err)
	}

	c.logf("  [conduit] status=%s", result.Status)
	return result.ConduitToken, nil
}

// getSentinelToken 获取 sentinel token（Step 2+3：prepare → PoW → finalize）
func (c *Client) getSentinelToken() (sentinelToken, proofToken string, err error) {
	reqToken := NewPOWConfig(c.userAgent).RequirementsToken()

	prepBody := map[string]string{
		"p": reqToken,
	}

	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "*/*",
			"Content-Type":          "application/json",
			"x-openai-target-path":  "/backend-api/sentinel/chat-requirements/prepare",
			"x-openai-target-route": "/backend-api/sentinel/chat-requirements/prepare",
		}).
		SetBody(prepBody).
		Post("/backend-api/sentinel/chat-requirements/prepare")
	if err != nil {
		return "", "", fmt.Errorf("sentinel/prepare request: %w", err)
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("sentinel/prepare %d: %s", resp.StatusCode, truncateStr(resp.String(), 200))
	}

	var pd struct {
		Persona     string `json:"persona"`
		Proofofwork *struct {
			Required   bool   `json:"required"`
			Seed       string `json:"seed"`
			Difficulty string `json:"difficulty"`
		} `json:"proofofwork"`
		Turnstile *struct {
			Required bool `json:"required"`
		} `json:"turnstile"`
		PrepareToken string `json:"prepare_token"`
	}
	if err := json.Unmarshal(resp.Bytes(), &pd); err != nil {
		return "", "", fmt.Errorf("parse sentinel/prepare: %w", err)
	}

	powRequired := pd.Proofofwork != nil && pd.Proofofwork.Required
	turnstileRequired := pd.Turnstile != nil && pd.Turnstile.Required
	c.logf("  [sentinel] persona=%s, PoW=%v, turnstile=%v", pd.Persona, powRequired, turnstileRequired)

	if powRequired {
		seed := pd.Proofofwork.Seed
		difficulty := pd.Proofofwork.Difficulty
		s0 := time.Now()

		proofToken = SolveProofToken(seed, difficulty, c.userAgent)
		c.logf("  [pow] solved in %dms", time.Since(s0).Milliseconds())
	}

	fb := map[string]interface{}{
		"prepare_token": pd.PrepareToken,
	}
	if proofToken != "" {
		fb["proofofwork"] = proofToken
	}

	finResp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"Accept":                "*/*",
			"Content-Type":          "application/json",
			"x-openai-target-path":  "/backend-api/sentinel/chat-requirements/finalize",
			"x-openai-target-route": "/backend-api/sentinel/chat-requirements/finalize",
		}).
		SetBody(fb).
		Post("/backend-api/sentinel/chat-requirements/finalize")
	if err != nil {
		return "", "", fmt.Errorf("sentinel/finalize request: %w", err)
	}
	if finResp.StatusCode != 200 {
		return "", "", fmt.Errorf("sentinel/finalize %d: %s", finResp.StatusCode, truncateStr(finResp.String(), 200))
	}

	var fd struct {
		Token       string `json:"token"`
		ExpireAfter int    `json:"expire_after"`
	}
	if err := json.Unmarshal(finResp.Bytes(), &fd); err != nil {
		return "", "", fmt.Errorf("parse sentinel/finalize: %w", err)
	}
	if fd.Token == "" {
		return "", "", fmt.Errorf("no sentinel token: %s", truncateStr(finResp.String(), 200))
	}

	c.logf("  [finalize] expire=%ds", fd.ExpireAfter)
	return fd.Token, proofToken, nil
}
