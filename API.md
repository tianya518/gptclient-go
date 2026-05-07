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

- **URL**: `/v1/chat/completions`
- **Method**: `POST`
- **Headers**:
  - `Content-Type: application/json`
  - `Authorization: Bearer <Token>` （免密池模式下可留空）

### 1.1 纯文本对话请求示例

```json
{
  "model": "gpt-5-5-thinking",
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
  "model": "gpt-4o",
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
| `model` | string | 否 | 指定使用的模型（见下方模型列表）。若不传，则默认使用服务端配置的 Default Model。 |
| `messages` | array | 是 | 历史对话数组，必须包含至少一条 `user` 角色的消息。 |
| `stream` | boolean | 否 | 是否启用 SSE 流式输出。强烈推荐设置为 `true`。 |

**扩展参数**：
- 如果请求体中传入了 `conversation_id`，服务端会自动将其与内部的 Session 进行绑定，实现上下文追溯和长会话保持。

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

### 当前支持模型

| 模型 ID | 说明 |
| :--- | :--- |
| `gpt-5-5-thinking` | GPT-5 Thinking 5.5，进阶推理（ChatGPT Plus 主推） |
| `gpt-5` | GPT-5 Instant 5.3，快速日常对话 |
| `gpt-4o` | GPT-4o，多模态旗舰（兼容保留） |
| `gpt-4o-mini` | GPT-4o mini，轻量快速（兼容保留） |
| `o3` | o3 推理模型 |
| `o4-mini` | o4-mini 轻量推理 |
| `o4-mini-high` | o4-mini 高算力推理 |

### 返回值示例

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-5-5-thinking",
      "object": "model",
      "created": 1714380000,
      "owned_by": "openai"
    },
    {
      "id": "gpt-4o",
      "object": "model",
      "created": 1714380000,
      "owned_by": "openai"
    }
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

## 6. 网页前端路由

Sentinel-Go 自身内置了轻量级的 Web UI 仪表盘和对话调试页面，可以直接在浏览器中访问。

- `GET /` ：服务仪表盘，可查看服务运行状态、上传/清空 Token。
- `GET /chat` ：自带的网页版多模态对话工具（支持免密池模式、切换模型、**Ctrl+V 粘贴图片上传**、实时流式文本对话等功能）。
