# Sentinel-Go API 文档

本文档介绍了 Sentinel-Go 提供的核心 API 接口。Sentinel-Go 实现了与 OpenAI 官方高度兼容的接口标准，方便通过各类兼容 OpenAI 规范的第三方客户端（如 Chatbox, NextChat 等）直接调用。

## 基础信息

- **默认端口**: `5005` (可配置)
- **Base URL**: `http://127.0.0.1:5005`
- **鉴权方式**（三种模式，优先级从高到低）：

| 模式 | `AUTHORIZATION` 环境变量 | 请求头 | 说明 |
| :--- | :--- | :--- | :--- |
| **密码模式** | 已设置 | `Bearer <配置的密码>` | 验证密码匹配后从 Token 池分配 ChatGPT Token |
| **直传模式** | 未设置 | `Bearer <ChatGPT accessToken>` | 直接将传入 Token 透传给 ChatGPT |
| **免密池模式** | 未设置 | 留空 或 `Bearer ` | 自动从已上传的 Token 池中轮询分配（推荐本地使用）|

> **accessToken 获取方式**：登录 [chatgpt.com](https://chatgpt.com) 后打开 `https://chatgpt.com/api/auth/session`，全选 `Ctrl+A` 复制整页内容，粘贴到仪表盘上传框即可（支持自动解析，无需手动截取 Token 字段）。

---

## 1. 聊天补全接口 (Chat Completions)

核心的对话交互接口，完全兼容 OpenAI `/v1/chat/completions` 格式，支持纯文本、流式传输以及多模态（图生文/图生图）请求。

> **流式产物与最终图片**：正文走 `delta.content`，生图/沙箱文件走 `sentinel` 侧信道，详见 [docs/CLIENT_STREAMING.md](docs/CLIENT_STREAMING.md)。

- **URL**: `/v1/chat/completions`
- **Method**: `POST`
- **Headers**:
  - `Content-Type: application/json`
  - `Authorization: Bearer <Token>` （免密池模式下可留空）

### 1.1 纯文本对话请求示例

```json
{
  "model": "gpt-5-5",
  "messages": [
    {
      "role": "system",
      "content": "你是一个有用的AI助手。"
    },
    {
      "role": "user",
      "content": "请给我写一首关于春天的诗。"
    }
  ],
  "stream": true
}
```

### 1.2 多模态（带图片）请求示例

支持将图片以 `Base64` 格式编码并嵌入在 `content` 数组中。

```json
{
  "model": "gpt-5-4",
  "messages": [
    {
      "role": "user",
      "content": [
        {
          "type": "text",
          "text": "这张图片里有什么？"
        },
        {
          "type": "image_url",
          "image_url": {
            "url": "data:image/jpeg;base64,/9j/4AAQSkZJRgABAQEAAAAAAAD..."
          }
        }
      ]
    }
  ],
  "stream": true
}
```

### 1.3 参数说明

| 字段名 | 类型 | 必填 | 描述 |
| :--- | :--- | :--- | :--- |
| `model` | string | 否 | 指定使用的模型（见下方模型列表）。若不传，则默认使用服务端配置的 `DEFAULT_MODEL`。 |
| `messages` | array | 是 | 历史对话数组，必须包含至少一条 `user` 角色的消息。 |
| `stream` | boolean | 否 | 是否启用 SSE 流式输出。强烈推荐设置为 `true`。 |

**扩展参数**：
- 如果请求体中传入了 `conversation_id`，服务端会自动将其与内部的 Session 进行绑定，实现上下文追溯和长会话保持。

### 1.3.1 图片生成（`dall-e-3`）

本服务**不走** OpenAI 官方的 `POST /v1/images/generations`，而是走 **ChatGPT 网页同源**对话：后端 `model` 为 **`dall-e-3`**，并自动注入 `system_hints: ["picture_v2"]`（与网页「画图」一致）。

| 字段 | 必填 | 说明 |
| :--- | :--- | :--- |
| `model` | 是（生图） | 推荐 **`dall-e-3`**（自动 `picture_v2`） |
| `messages` | 是 | 至少一条 `user`，`content` 为画图描述（可附参考图 `image_url`） |
| `stream` | 建议 `true` | 图片通过 SSE 的 `sentinel` 侧信道或 `artifact_markdown` 返回 |
| `size` | 否 | 宽高比：`1:1` / `3:4` / `9:16` / `4:3` / `16:9`（或 `1024x1024` 等，会映射为比例） |
| `artifact_delivery` | 否 | `url`（默认，代理链接）/ `base64` / `base64_chunked` |
| `artifact_markdown` | 否 | `true` 时在正文末尾追加 `![Generated Image](...)` |
| `conversation_id` | 否 | 多轮修图时带上轮次返回的 ID |

**无需**单独传 `picture_v2`；`model` 含 `dall-e` 时会自动开启。

请求示例：

```json
{
  "model": "dall-e-3",
  "messages": [{ "role": "user", "content": "画一只在沙发上的橘猫，卡通风格" }],
  "stream": true,
  "size": "1:1",
  "artifact_delivery": "url"
}
```

> **兼容别名**：若客户端仍传 `gpt-image-2` / `gpt-image-2-thinking`，服务端会**映射为 `dall-e-3`** 再请求 ChatGPT（响应里 `model` 仍回显你传入的名称）。这与 OpenAI API 独立的 `gpt-image-2` 图像端点不是同一路径。

### 1.4 返回值格式 (Stream = true)

标准的 SSE (Server-Sent Events) 流式返回：

```text
data: {"id":"chatcmpl-uuid","object":"chat.completion.chunk","created":1714382928,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-uuid","object":"chat.completion.chunk","created":1714382928,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"这是"},"finish_reason":null}]}

...

data: [DONE]
```

如果生成了图片（如图生图），服务端会在流中自动插入 Markdown 格式的图片标签：
```text
data: {"id":"chatcmpl-uuid","object":"chat.completion.chunk","created":1714382928,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"\n\n![Generated Image](/api/image/proxy?conv_id=xxx&file_id=yyy)"},"finish_reason":null}]}
```

如果发生错误，服务端也会通过 SSE 流返回错误信息（HTTP 状态码仍为 200）：
```text
data: {"error":{"message":"get conduit token: 401 unauthorized","type":"server_error"}}
```

---

## 2. 模型列表获取 (Models)

兼容 OpenAI 的模型列表获取接口，常用于第三方客户端自动获取支持的模型。

- **URL**: `/v1/models`
- **Method**: `GET`
- **Headers**:
  - `Authorization: Bearer <Token>` （免密池模式下可留空）

### 当前支持模型（2026-07-09 MCP 抓包对齐）

| 模型 ID（调用方传入） | 后端真实 model | thinking_effort | 说明 |
| :--- | :--- | :--- | :--- |
| `gpt-5-5-thinking`（**默认**） | `gpt-5-5-thinking` | `extended` | 官网"高级"，深度思考，支持多图并行 |
| `gpt-5-5` | `gpt-5-5` | （不携带） | 官网"极速"，无思考，最快响应 |
| `gpt-5-4` / `gpt-5-4-thinking` | `gpt-5-4-thinking` | `standard` | 官网 GPT-5.4，上一代 thinking 模型 |
| `gpt-5-3` / `gpt-5-3-instant` | `gpt-5-3-instant` | （不携带） | 官网 GPT-5.3 极速，最轻量 |
| `o3` | `o3` | （不携带） | 推理专用模型 |
| `dall-e-3` / `gpt-image-2*` | `dall-e-3` | n/a | **图片生成专用**，自动开启 `picture_v2` |

> **说明**：
> - 调用方传入友好别名（如 `gpt-5-4`），`ResolveChatModel` 自动映射到后端 model 和 `thinking_effort`，无需手动指定。
> - `thinking_effort: "extended"` 是触发多图并行生成的关键参数（官网"高级"模式默认值）。
> - 极速/o3/gpt-5-3-instant 等官网原生不发送 `thinking_effort` 字段，服务端对齐此行为。

### 返回值示例

```json
{
  "object": "list",
  "data": [
    {"id": "gpt-5-5-thinking", "object": "model", "created": 1751000000, "owned_by": "openai"},
    {"id": "gpt-5-5",          "object": "model", "created": 1751000000, "owned_by": "openai"},
    {"id": "gpt-5-4-thinking", "object": "model", "created": 1751000000, "owned_by": "openai"},
    {"id": "gpt-5-3-instant",  "object": "model", "created": 1751000000, "owned_by": "openai"},
    {"id": "o3",               "object": "model", "created": 1751000000, "owned_by": "openai"},
    {"id": "dall-e-3",         "object": "model", "created": 1751000000, "owned_by": "openai"}
  ]
}
```

---

## 3. 图片渲染代理 (Image Proxy)

由于 OpenAI 的图片直链（尤其是内部 Estuary 链接或防盗链 CDN）直接在前端请求会报 `403 Forbidden` 错误。Sentinel-Go 提供了智能的图片内存代理接口。

- **URL**: `/api/image/proxy`
- **Method**: `GET`
- **说明**: 这是一个前端渲染专用接口。大模型返回的图片 Markdown 语法会自动指向这个接口，前端渲染时无需进行特殊处理。

### 3.1 参数说明 (Query Params)

| 字段名 | 类型 | 必填 | 描述 |
| :--- | :--- | :--- | :--- |
| `conv_id` | string | 是 | 对应的会话 ID (Conversation ID)。用于在后端寻找对应的 Session 以获取鉴权凭证。 |
| `file_id` | string | 是 | 图片的文件 ID (`file_` 开头)。 |

### 3.2 代理机制
- 服务端会利用对应的 Session 获取最新的官方签名直链。
- 根据直链类型（`chatgpt.com` 内部链接 或 外部 CDN 链接），智能动态附加对应的 `Authorization` Header，从服务端拉取图片流。
- 通过透明管道直接输出到前端的 `<img>` 标签中。

---

## 4. Token 管理接口

### 4.1 上传 Token

- **URL**: `/tokens/upload`
- **Method**: `POST`
- **Content-Type**: `text/plain` 或 `application/x-www-form-urlencoded`（字段名 `tokens`）
- **说明**: 支持两种格式（可混合，每行一条）：
  1. 直接粘贴 JWT 格式的 `accessToken`（`eyJ` 开头）
  2. 粘贴 `chatgpt.com/api/auth/session` 返回的完整 JSON（系统自动提取 `accessToken` 字段）

### 4.2 清空 Token 池

- **URL**: `/tokens/clear`
- **Method**: `POST`

### 4.3 查看失效 Token

- **URL**: `/tokens/errors`
- **Method**: `GET`

---

## 5. 健康检查 (Health Check)

- **URL**: `/health`
- **Method**: `GET`

### 返回值示例

```json
{
  "status": "ok",
  "tokens_total": 3,
  "tokens_valid": 2,
  "uptime": "1h23m"
}
```

---

## 6. 根路径与静态资源

内置的网页前端（仪表盘 / 对话调试页）已移除。推荐使用 **Open WebUI** 等任意 OpenAI 兼容客户端，将 API 地址指向 `/v1` 即可。

- `GET /` ：返回服务信息 JSON（服务名与可用端点列表）。
- `GET /images/*` ：DALL·E 生成图片的静态资源目录。
