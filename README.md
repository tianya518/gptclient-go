# sentinel-go

> 用 Go 实现的 ChatGPT Web 端逆向客户端，无需 OpenAI API Key。  
> 暴露标准 **OpenAI 兼容接口**，可直接接入 Open WebUI、Cherry Studio、NextChat、Cursor 等任意客户端。

---

## 目录

- [核心能力](#核心能力)
- [5 分钟快速上手](#5-分钟快速上手)
- [Token 获取与管理](#token-获取与管理)
- [对接前端 / 第三方客户端](#对接前端--第三方客户端)
- [模型说明](#模型说明)
- [环境变量完整参考](#环境变量完整参考)
- [Token 管理 API](#token-管理-api)
- [Docker 部署](#docker-部署)
- [作为 Go 库使用](#作为-go-库使用)
- [项目结构](#项目结构)
- [注意事项](#注意事项)

---

## 核心能力

| 能力 | 说明 |
|---|---|
| 文本对话 | 多轮、流式、思考过程显示 |
| **多图并行生成** | gpt-5-5-thinking 模式，一次请求生成 N 张独立图片 |
| 图生图 | 上传参考图 + 改图描述，返回编辑后图片 |
| 文件上传分析 | PDF、TXT、代码等文档上传给 AI 分析 |
| 文件下载 | AI 生成的 PDF/代码 文件直接下载 |
| Token 自动刷新 | 支持 session_token / refresh_token 换新，后台定时维护 |
| 多 Token 池 | 多账号轮换，401 自动切换，支持并发 |
| OpenAI 兼容接口 | `/v1/chat/completions` + `/v1/models`，无缝接入现有工具链 |

---

## 5 分钟快速上手

### 前置条件

- Go 1.21+（或直接用 Docker）
- 一个已登录 ChatGPT Plus 的账号

### 第一步：获取 Token

1. 登录 [chatgpt.com](https://chatgpt.com)
2. 在同一浏览器打开 [chatgpt.com/api/auth/session](https://chatgpt.com/api/auth/session)
3. 页面显示一段 JSON，全选 `Ctrl+A` 复制
4. 这就是你的 Token（支持粘贴整段 JSON，程序自动提取 `accessToken`）

### 第二步：启动服务

```bash
git clone https://github.com/tianya518/gptclient-go.git sentinel-go
cd sentinel-go

# 写入 Token（替换引号内的内容为你复制的 JSON 或 JWT）
echo '{"bearerToken": "eyJhbGci..."}' > config.json

# 启动 API 服务器（默认监听 :5005）
go run ./cmd/server/
```

> **或使用 Docker（推荐生产）**：见 [Docker 部署](#docker-部署)

### 第三步：验证服务

```bash
# 健康检查
curl http://localhost:5005/health

# 发一条对话
curl -s http://localhost:5005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-5-thinking","messages":[{"role":"user","content":"你好"}],"stream":false}' \
  | python -m json.tool
```

看到 `choices[0].message.content` 有内容即成功 ✅

---

## Token 获取与管理

### 上传 Token 到服务器

服务启动后，通过接口上传 Token（支持多种格式，见下表）：

```bash
# 上传整段 session JSON（推荐，携带 session_token 可自动刷新）
curl -s -X POST http://localhost:5005/tokens/upload \
  -H "Content-Type: application/json" \
  -d '{"tokens": "{\"accessToken\":\"eyJhbGci...\",\"user\":{...}}"}'

# 或只上传 access_token（不能自动刷新）
curl -s -X POST http://localhost:5005/tokens/upload \
  -H "Content-Type: application/json" \
  -d '{"tokens": "eyJhbGci..."}'
```

**支持的 Token 格式（每行一条，或整段上传）：**

| 格式 | 示例 | 能否自动刷新 |
|---|---|---|
| 整段 session JSON | 从 `/api/auth/session` 复制 | ✅（携带 session_token） |
| access_token + session_token | `eyJhbGci...----eyJhbGciOiJkaXIi...`（4个`-`分隔） | ✅ |
| refresh_token（OAuth） | `rt-...` 开头的字符串 | ✅ |
| 纯 access_token | `eyJhbGci...` | ❌（10天内有效） |
| 纯 session_token | `st-...` 开头 | ✅ |

**查看 Token 池状态：**

```bash
curl http://localhost:5005/tokens
```

### Token 有效期与自动刷新

| Token 类型 | 有效期 | 说明 |
|---|---|---|
| access_token | ~10 天 | 直接用于 API 请求 |
| session_token | 数月 | 可换新 access_token（需从同 IP 刷新，建议配置代理） |
| refresh_token | 更长 | OAuth 换 access_token，不受 IP 限制 |

服务器默认在 AT 过期前 **1 天**自动刷新，每 **30 分钟**检查一次。可通过环境变量调整：

```bash
TOKEN_REFRESH_AHEAD_SEC=86400   # 提前 1 天刷新（默认）
REFRESH_LOOP_SEC=1800           # 每 30 分钟检查（默认）
```

---

## 对接前端 / 第三方客户端

任何支持自定义 OpenAI API 地址的客户端均可直接对接。

### Open WebUI（推荐）

1. 启动 sentinel-go 服务（`:5005`）
2. 进入 Open WebUI → **Settings → Connections → OpenAI API**
3. 填写：

| 项 | 值 |
|---|---|
| API Base URL | `http://你的IP:5005/v1` |
| API Key | 留空，或填你设置的 `AUTHORIZATION` 值 |

4. 刷新模型列表，选择 `gpt-5-5-thinking` 开始对话

### Cherry Studio / NextChat / Chatbox

在 API 设置中填写：

| 项 | 值 |
|---|---|
| 接口地址 | `http://localhost:5005` |
| API Key | 留空（未设置 `AUTHORIZATION` 时）或你的密码 |
| 默认模型 | `gpt-5-5-thinking` |

### Cursor（作为 AI 补全后端）

在 Cursor → Settings → Models → OpenAI API Key 处：

| 项 | 值 |
|---|---|
| Override OpenAI Base URL | `http://localhost:5005/v1` |
| API Key | 任意字符串（或 `AUTHORIZATION` 值） |

### 鉴权模式说明

| `AUTHORIZATION` 配置 | 客户端传入 | 行为 |
|---|---|---|
| **未设置**（默认） | 不传 | 从 Token 池自动分配（推荐本地使用） |
| **未设置** | `Bearer <access_token>` | 直接用传入的 JWT 访问 ChatGPT |
| **已设置密码** | `Bearer <你的密码>` | 验证密码后从 Token 池分配 |

---

## 模型说明

基于 2026-07-09 官网 MCP 实测抓包，模型与后端参数对应关系：

| 推荐传入（model 字段） | 实际后端 | thinking_effort | 适用场景 |
|---|---|---|---|
| `gpt-5-5-thinking` **（默认）** | gpt-5-5-thinking | extended | 最强，支持多图并行，深度推理 |
| `gpt-5-5` | gpt-5-5 | 无 | 极速响应，简单问答 |
| `gpt-5-4` | gpt-5-4-thinking | standard | 均衡，上一代 thinking |
| `gpt-5-4-thinking` | gpt-5-4-thinking | extended | 5.4 深度思考 |
| `gpt-5-3` / `gpt-5-3-instant` | gpt-5-3-instant | 无 | 最轻量极速 |
| `o3` | o3 | 无 | 推理专用 |
| `dall-e-3` | dall-e-3 | — | **图片生成**（自动启用 picture_v2） |

> `gpt-image-2` / `gpt-image-2-thinking` 等别名会自动映射到 `dall-e-3`。

### 多图生成示例

```bash
curl -s http://localhost:5005/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-5-5-thinking",
    "messages": [{"role":"user","content":"生成6张不同风格的猫的图片，写实、油画、水彩、像素、卡通、赛博朋克各一张"}],
    "stream": true
  }'
```

---

## 环境变量完整参考

| 变量 | 默认值 | 说明 |
|---|---|---|
| `PORT` | `5005` | HTTP 监听端口 |
| `AUTHORIZATION` | `""` | API 密码（空=不校验） |
| `DEFAULT_MODEL` | `gpt-5-5-thinking` | 请求未指定 model 时的默认模型 |
| `TEMP_MODE` | `false` | 临时模式（对话不保存历史） |
| `IMAGE_DIR` | `images` | 生成图片保存目录 |
| `TOKENS_FILE` | `tokens.json` | Token 池持久化文件 |
| `SESSION_TTL_MINUTES` | `120` | Session 不活跃超时（分钟） |
| `BASE_URL` | `""` | 对外绝对地址（生成图片代理 URL 用），如 `https://your.domain` |
| `PROXY_URL` | `""` | 出站代理，如 `socks5://127.0.0.1:1080`（访问 chatgpt.com 用） |
| `TOKEN_REFRESH_AHEAD_SEC` | `86400` | AT 过期前提前刷新秒数（默认 1 天） |
| `REFRESH_LOOP_SEC` | `1800` | 后台刷新检查间隔（秒，0=关闭） |
| `OAUTH_TOKEN_URL` | `""` | refresh_token 换 AT 的 OAuth 端点（空=默认） |
| `OAUTH_CLIENT_ID` | `""` | OAuth Client ID（空=默认） |
| `SENTINEL_ALLOW_IPV6` | `""` | 非空则允许 IPv6 出站（默认强制 IPv4 防 TLS 卡顿） |

---

## Token 管理 API

| 接口 | 方法 | 说明 |
|---|---|---|
| `/tokens` | GET | 查看 Token 池状态（数量、过期时间等） |
| `/tokens/upload` | POST | 批量上传 Token，body: `{"tokens":"..."}` |
| `/tokens/add/:token` | GET | 添加单个 Token |
| `/tokens/clear` | POST | 清空 Token 池 |
| `/tokens/errors` | GET | 查看失效 Token 列表 |
| `/tokens/check` | GET | 检测是否有可用 Token |
| `/health` | GET | 健康检查 |
| `/v1/models` | GET | 模型列表 |
| `/v1/chat/completions` | POST | 对话（OpenAI 兼容） |
| `/api/image/proxy` | GET | 生成图片代理（前端直接访问） |
| `/api/pdf/proxy` | GET | AI 生成文件代理下载 |

---

## Docker 部署

### 基础启动

```bash
docker build -t sentinel-go .
docker run -d \
  --name sentinel-go \
  -p 5005:5005 \
  -e AUTHORIZATION="your-password" \
  -e PROXY_URL="socks5://host:1080" \
  -v $(pwd)/tokens.json:/app/tokens.json \
  -v $(pwd)/images:/app/images \
  sentinel-go
```

### docker-compose（推荐）

```yaml
# docker-compose.yml
services:
  sentinel-go:
    build: .
    ports:
      - "5005:5005"
    environment:
      AUTHORIZATION: "your-password"       # API 访问密码
      DEFAULT_MODEL: "gpt-5-5-thinking"
      PROXY_URL: "socks5://127.0.0.1:1080" # 按需配置
      TOKEN_REFRESH_AHEAD_SEC: "86400"
      REFRESH_LOOP_SEC: "1800"
      BASE_URL: "https://your.domain"      # 有公网域名时填写
    volumes:
      - ./tokens.json:/app/tokens.json
      - ./images:/app/images
    restart: unless-stopped
```

```bash
docker compose up -d
docker compose logs -f  # 查看日志
```

服务启动后访问 `http://localhost:5005/health` 确认运行正常，然后通过 `/tokens/upload` 接口上传 Token。

---

## 作为 Go 库使用

```go
import sentinel "sentinel-go/sentinel"

client := sentinel.NewClient(sentinel.Config{
    BearerToken: "eyJhbGci...",
    ProxyURL:    "socks5://127.0.0.1:1080", // 可选
    Model:       "gpt-5-5-thinking",
})

// 流式对话
result, err := client.ChatStream(sentinel.ChatOptions{
    Text: "用 Go 写一个 HTTP 服务器",
}, func(delta string) {
    fmt.Print(delta)
})
fmt.Println(result.Text)

// 多轮对话（自动维护上下文）
client.Chat(sentinel.ChatOptions{Text: "我叫张三"})
result, _ = client.Chat(sentinel.ChatOptions{Text: "我叫什么？"}) // → 张三

// 图生图（传入 base64 或 URL 图片）
uf, _ := client.UploadFile(ctx, imageBytes, "ref.png", "image/png")
result, _ = client.Chat(sentinel.ChatOptions{
    Text:   "给这只猫戴上圣诞帽",
    Images: []sentinel.UploadedFile{*uf},
})

// 强制生图（picture_v2，指定宽高比）
result, _ = client.Chat(sentinel.ChatOptions{
    Text:           "一只在海边的猫",
    ForcePictureV2: true,
    ImageAspect:    sentinel.ImageAspectLandscape,
})

// 重置对话 / 切换模型
client.ResetSession()
client.SetModel("gpt-5-4")
```

### Config 字段

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `BearerToken` | string | ✅ | ChatGPT access_token（JWT） |
| `ProxyURL` | string | ❌ | 出站代理（socks5/http） |
| `Model` | string | ❌ | 默认 `gpt-5-5-thinking` |
| `CookieString` | string | ❌ | 浏览器 Cookie（可选增强） |
| `ImageDir` | string | ❌ | 图片下载目录，默认 `images/` |
| `TempMode` | bool | ❌ | 临时模式，不保存历史 |
| `UserAgent` | string | ❌ | 默认模拟 Chrome 149 |
| `Language` | string | ❌ | 默认 `zh-CN` |

---

## 项目结构

```
sentinel-go/
├── sentinel/               # 核心协议库
│   ├── client.go           # Client 结构体 & HTTP 初始化
│   ├── auth.go             # Sentinel 三步认证（conduit+PoW+token）
│   ├── chat.go             # 对话入口与请求编排
│   ├── chat_stream.go      # SSE 主读取循环与分发
│   ├── chat_sse.go         # SSE 事件解析
│   ├── chat_ws.go          # WebSocket 连接与帧处理
│   ├── image_handoff.go    # 生图路由探测（轮询 vs WS）
│   ├── image_poll.go       # HTTP 轮询 async_status 收图
│   ├── image_revision.go   # 图片版本管理
│   ├── image_ws.go         # WS 生图异步跟踪
│   ├── image_parse.go      # image_asset_pointer 解析
│   ├── image_state.go      # 生图状态查询工具
│   ├── files.go            # 文件三步上传（Azure Blob）
│   ├── pdf.go              # PDF/沙箱文件下载
│   ├── model_resolve.go    # model 别名 → 后端参数映射
│   ├── artifact.go         # 产物信号检测
│   └── types.go / utils.go
├── server/                 # OpenAI 兼容 HTTP 服务
│   ├── handler_chat.go         # /v1/chat/completions 入口
│   ├── handler_chat_input.go   # 消息解析
│   ├── handler_chat_stream.go  # SSE/非流式响应
│   ├── handler_proxy.go        # 图片/文件代理
│   ├── handler_models.go       # /v1/models
│   ├── handler_tokens.go       # Token 管理 API
│   ├── token_pool.go           # Token 池（刷新、轮换）
│   ├── token_store.go          # Token 持久化
│   └── config.go / session.go / router.go
├── cmd/
│   ├── chat/main.go        # CLI 交互式 REPL
│   ├── server/main.go      # API 服务器启动入口
│   └── stream-capture/     # SSE 原始流抓取工具
├── docs/
│   ├── IMAGE_FLOW_CAPTURE.md   # 官网协议抓包记录
│   └── PROTOCOL_BASELINE.md
├── config.json             # 本地凭证（勿提交 Git）
├── tokens.json             # Token 池（勿提交 Git）
├── Dockerfile
└── docker-compose.yml
```

---

## 注意事项

- 本项目仅供学习与研究，请勿用于违反 OpenAI 服务条款的场景
- Bearer Token 是个人凭证，**不要将 `config.json` / `tokens.json` 提交到公开仓库**
- session_token 刷新受 IP 限制，若服务器 IP 与 Token 获取 IP 不同，建议配置 `PROXY_URL`
- `.gitignore` 建议包含：`config.json`、`tokens.json`、`tokens.txt`、`images/`

---

## License

MIT
