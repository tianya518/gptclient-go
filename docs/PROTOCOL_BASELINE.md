# ChatGPT 官网接口基线（MCP 实抓）

> 抓取时间：2026-07-07，Chrome 149，账号 Plus。
> 目的：核对官网当前接口/字段/流程是否与 sentinel-go 代码一致，作为"同步流程"依据。
> 约定：**协议头 / 风控 / PoW / Turnstile / UA 一律保持代码现状不动**，本文档仅记录事实。

---

## 场景一：纯文本对话（模型 gpt-5-5-thinking）

### 请求时序（fetch，`/backend-api/` 过滤）

1. `POST /backend-api/f/conversation/prepare`  → 返回 conduit_token
2. `POST /backend-api/sentinel/chat-requirements/prepare` → 返回 prepare_token + turnstile
3. `POST /backend-api/sentinel/ping`（**新增，代码中无**，心跳，无 body/无响应体）
4. `POST /backend-api/sentinel/chat-requirements/finalize` → 返回 sentinel token
5. `POST /backend-api/f/conversation`（主对话，SSE 流）
6. `GET /backend-api/conversation/{id}/stream_status`
7. 其余为遥测/侧栏：`sentinel/ping`、`lat/r`、`beacons/home`、`conversations?...`、`textdocs` 等

### 关键请求：`POST /backend-api/f/conversation` body（真实）

```json
{
  "action": "next",
  "messages": [{
    "id": "<uuid>",
    "author": {"role": "user"},
    "create_time": 1783425435.523,
    "content": {"content_type": "text", "parts": ["用一句话介绍你自己"]},
    "metadata": {"selected_sources": [], "serialization_metadata": {"custom_symbol_offsets": []}}
  }],
  "parent_message_id": "client-created-root",
  "model": "gpt-5-5-thinking",
  "client_prepare_state": "none",
  "timezone_offset_min": -480,
  "timezone": "Asia/Shanghai",
  "conversation_mode": {"kind": "primary_assistant"},
  "enable_message_followups": true,
  "system_hints": [],
  "supports_buffering": true,
  "supported_encodings": ["v1"],
  "client_contextual_info": {
    "is_dark_mode": false, "time_since_loaded": 227,
    "page_height": 861, "page_width": 929, "pixel_ratio": 1,
    "screen_height": 1080, "screen_width": 1920,
    "app_name": "chatgpt.com",
    "has_web_push_capabilities": true,
    "web_push_notification_permission": "default"
  },
  "paragen_cot_summary_display_override": "allow",
  "force_parallel_switch": "auto",
  "thinking_effort": "extended"
}
```

### 与代码 body 的差异（新增字段，非风控）

官网当前比代码 `buildConversationBody`（sentinel/chat.go:70-158）多出：

- `client_prepare_state: "none"`
- `paragen_cot_summary_display_override: "allow"`
- `force_parallel_switch: "auto"`
- `thinking_effort: "extended"`（thinking 系模型）
- `client_contextual_info` 内多 `app_name` / `has_web_push_capabilities` / `web_push_notification_permission`
- `metadata.serialization_metadata.custom_symbol_offsets`

> 结论：这些字段缺失不影响当前可用性（服务端有默认值）；可选补充以更贴近官网。**endpoint 与核心结构（action/messages/model/parent_message_id）完全未变。**

---

## 认证三步（endpoint 未变，内部有变化 —— 按约定不改）

### Step 1 `POST /backend-api/f/conversation/prepare`

- 请求 body：空（官网未带 body）。代码当前带 `partial_query`（前 5 字符），差异记录，不动。
- 响应：`{"status":"ok","conduit_token":"<JWT>"}`
  - JWT payload 含 `conduit_uuid` / `conduit_location` / `cluster` / `iat` / `exp` / `turn_topic_id`

### Step 2 `POST /backend-api/sentinel/chat-requirements/prepare`

- 响应：`{"persona":"chatgpt-paid","prepare_token":"gAAAAAB...","turnstile":{"required":true,"dx":"<大段数据>"}}`
- **重要变化：响应中已无 `proofofwork` 字段**（代码 sentinel/auth.go 仍解析 `proofofwork.seed/difficulty/required`）。
  - 即官网此接口当前不下发 PoW 挑战，只给 `prepare_token` + `turnstile`。
- `turnstile.required = true`，但当前代码不处理 turnstile 仍可正常对话 → 视为软要求 / 不阻断。**按约定不改。**

### Step 3 `POST /backend-api/sentinel/chat-requirements/finalize`

- 响应：`{"persona":"chatgpt-paid","token":"gAAAAAB...","expire_after":540,"expire_at":1783425978}`
- **`expire_after: 540`（秒）+ `expire_at`（unix 秒）**：可作为 sentinel token 缓存有效期依据（代码目前每轮重新 prepare+finalize）。

### 新接口 `POST /backend-api/sentinel/ping`

- 心跳，POST，无 body、无响应体（推测 204）。代码中无对应逻辑，不影响核心流程。

---

## 待补充场景

- [ ] 场景二：文生图（同时多张）——system_hints / async_status / mapping / 是否有"预期张数"字段
- [ ] 场景三：图生图（参考图区分）
- [ ] 场景四：文件上传 + 总结
- [ ] 场景五：生成 PDF + 下载
