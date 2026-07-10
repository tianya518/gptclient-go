# sentinel-go

> 用 Go 语言逆向实现的 ChatGPT Web 端非官方客户端库，无需 OpenAI API Key，直接使用浏览器 Bearer Token 与 ChatGPT 对话。支持作为 **CLI 工具**或**本地 OpenAI 兼容 API 服务器**使用。

---

## 特性

- ✅ 完整实现 ChatGPT Web 端 Sentinel 认证流程（conduit token + SHA3-512 PoW + sentinel token）
- ✅ WebSocket 流式输出（实时回调增量文本，低延迟）
- ✅ 多轮对话（自动维护 conversation_id / parent_message_id）
- ✅ DALL-E 图片生成（自动识别生图请求，实时显示思考过程，自动下载到本地）
- ✅ 多模态输入（上传图片到对话）
- ✅ 文件上传（PDF、文档等）
- ✅ 临时模式（不保存对话历史 / 不更新记忆）
- ✅ 浏览器指纹伪装（TLS 指纹 + Edge 146 UA + 完整 sec-ch-ua Headers）
- ✅ OpenAI 兼容 API 服务器（`/v1/chat/completions`）+ 多 Token 池轮换
- ✅ 流式产物侧信道（`sentinel`：生图多版本、沙箱文件）— 见 [docs/CLIENT_STREAMING.md](docs/CLIENT_STREAMING.md)
- ✅ 开箱即用的交互式 CLI（REPL）

---

## 项目结构

```
sentinel-go/
├── sentinel/           # 核心协议库（package sentinel）
│   ├── types.go            # 公开类型定义（Config、ChatResult、StreamHandler 等）
│   ├── client.go           # Client 核心结构体 & HTTP 客户端初始化
│   ├── auth.go             # Sentinel 三步认证（conduit + PoW + sentinel）
│   ├── pow.go              # SHA3-512 Proof-of-Work 算法实现
│   ├── chat.go             # 对话主流程（SSE + WebSocket 流式处理）
│   ├── image.go            # DALL-E 图片轮询、下载、HTTP 代理
│   ├── files.go            # 文件三步上传（Azure Blob + ChatGPT 注册）
│   └── utils.go            # UUID、工具函数
├── server/             # OpenAI 兼容 HTTP 服务（package server）
├── cmd/
│   ├── chat/main.go        # CLI 交互式 REPL 入口
│   └── server/main.go      # OpenAI 兼容 API 服务器入口
├── config.json         # 本地凭证配置（不要提交到 Git）
├── Dockerfile
└── docker-compose.yml
```

---

## 快速开始 — CLI 模式

### 1. 获取 Bearer Token

1. 登录 [https://chatgpt.com](https://chatgpt.com)
2. 在同一浏览器中打开 [https://chatgpt.com/api/auth/session](https://chatgpt.com/api/auth/session)
3. 页面会显示一段 JSON，全选（`Ctrl+A`）后复制
4. 将**完整 JSON** 粘贴到 `config.json` 的 `bearerToken` 字段，程序会自动提取其中的 `accessToken`

```json
{
  "bearerToken": "{\"user\":{...},\"accessToken\":\"eyJhbGci...\"}",
  "cookieString": ""
}
```

> 也可以只填纯 JWT 字符串（`eyJhbGci...`），两种格式都支持。
>
> ⚠️ Token 有效期约 **10 天**，过期后重新打开该页面获取即可。

### 2. 运行

```bash
# 交互式多轮对话（REPL 模式）
go run ./cmd/chat/

# 单次问答
go run ./cmd/chat/ "你好，介绍一下自己"

# 指定模型
go run ./cmd/chat/ -model gpt-5-4 "帮我写一段 Go 代码"

# 临时模式（不保存历史）
go run ./cmd/chat/ -temp
```

### CLI 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-config` | `config.json` | 配置文件路径 |
| `-model` | `gpt-5-5` | 使用的模型名称 |
| `-temp` | `false` | 开启临时模式（不保存对话历史） |

### REPL 内置命令

| 命令 | 说明 |
|------|------|
| `/new` | 开启新对话，清空上下文 |
| `/model <name>` | 切换模型（不传参数则显示当前模型） |
| `/temp` | 切换临时模式开/关 |
| `/info` | 查看当前会话详情（conversation_id、model、轮次等） |
| `/exit` / `/quit` | 退出程序 |

**当前可选模型：**

```
gpt-5-5
gpt-5-4
gpt-5-3
o3
```

---

## 快速开始 — API 服务器模式

将 sentinel-go 作为本地 **OpenAI 兼容 API 服务器**运行，可直接接入 Cherry Studio、Open WebUI、NextChat、Cursor 等任意支持自定义 API 地址的客户端。

### 启动服务器

```bash
go run ./cmd/server/
```

默认监听 `http://localhost:5005`，接口路径：
- `POST /v1/chat/completions`
- `GET  /v1/models`

### 配置客户端

在你的 AI 客户端中填写：

| 项目 | 值 |
|------|----|
| API Base URL | `http://localhost:5005` |
| API Key | 留空（或填任意值，视鉴权配置而定） |
| Model | `gpt-5-5`（或其他支持的模型） |

### Docker 部署

**直接运行：**

```bash
docker build -t sentinel-go .
docker run -d \
  -p 5005:5005 \
  -e AUTHORIZATION="your-api-key" \
  -v $(pwd)/tokens.json:/app/tokens.json \
  -v $(pwd)/images:/app/images \
  sentinel-go
```

**使用 docker-compose：**

```bash
# 编辑 docker-compose.yml 中的环境变量后：
docker compose up -d
```

### 服务器环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `5005` | 监听端口 |
| `AUTHORIZATION` | `""` | API 鉴权 Key（留空则不校验，直接用传入 token 作为 ChatGPT token） |
| `DEFAULT_MODEL` | `gpt-5-5` | 默认模型 |
| `TEMP_MODE` | `false` | 临时模式（不保存对话历史） |
| `IMAGE_DIR` | `images` | 图片保存目录 |
| `TOKENS_FILE` | `tokens.json` | Token 持久化文件路径（JSON，含 access + session） |
| `SESSION_TTL_MINUTES` | `120` | Session 不活跃超时（分钟） |

### Token 管理

服务器支持多 Token 轮换，通过管理面板（`http://localhost:5005`）或以下接口管理：

| 接口 | 说明 |
|------|------|
| `GET  /tokens` | 查看 Token 池状态 |
| `POST /tokens/upload` | 批量上传 Token（JSON body: `{"tokens":"eyJ..."}`) |
| `GET  /tokens/add/:token` | 添加单个 Token |
| `POST /tokens/clear` | 清空 Token 池 |
| `GET  /tokens/errors` | 查看失效 Token 列表 |
| `GET  /health` | 健康检查 |

**`tokens.json` 持久化格式：**

```json
{
  "version": 1,
  "tokens": [
    {
      "id": "a1b2c3",
      "access_token": "eyJhbGciOiJS...",
      "session_token": "eyJhbGciOiJkaXIi...",
      "expires_at": "2026-08-29T10:19:02Z",
      "updated_at": "2026-05-31T12:00:00Z"
    }
  ]
}
```

**上传/import 支持的文本格式（每行一条，或整段 session JSON）：**

| 格式 | 示例 |
|------|------|
| 仅 Access | `eyJhbGciOiJS...` |
| Access + Session | `eyJhbGciOiJS...----eyJhbGciOiJkaXIi...`（四个 `-`） |
| 仅 Session | `eyJhbGciOiJkaXIi...` 或 `st:...` |
| Session API JSON | 从 `/api/auth/session` 复制的整段 JSON |

首次启动若仅有旧版 `tokens.txt`（按行 `st:` / JWT），会自动迁移到 `tokens.json`。

配置 `TOKEN_REFRESH_AHEAD_SEC`（默认 86400）可在 AT 过期前提前用 ST/RT 换新；刷新后会写回 JSON。池模式请求若遇 401 会自动尝试刷新一次。

### 产物检测与流式抓包（stream-capture）

对话结束后是否轮询**图片 / 沙箱文件（pdf、txt 等）**，由 SSE 与对话 JSON 中的**结构化信号**决定（如 `image_gen_task_id`、`author.name=python`、`execution_output`、`/mnt/data/...`），**不使用用户/助手文本关键词**。

抓取三种场景原始流式数据以便分析、调参：

```bash
go run ./cmd/stream-capture/ -config config.json
# 仅跑某一类: -case image | txt | pdf
```

输出目录 `testdata/stream-captures/<时间>-<case>/`：

| 文件 | 内容 |
|------|------|
| `sse.ndjson` | 每条 SSE 的 event、data、解析出的 signals |
| `conversation.json` | 对话 mapping 全量 |
| `analysis.json` | 信号汇总与 `ArtifactPlan` |
| `summary.txt` | 人类可读摘要 |

### API 兼容性说明

与标准 OpenAI API 的主要差异：

| 项目 | 说明 |
|------|------|
| 多轮对话 | 上下文由服务端 Session 维护，建议每次请求携带响应中返回的 `conversation_id` |
| `usage` 字段 | 全部为 0（逆向无法统计 token 用量） |
| `temperature` / `max_tokens` | 接收但不生效 |
| 图片生成 | 发送生图请求会自动触发 DALL-E，图片以 Markdown 格式 `![](url)` 追加在回复末尾 |
| `conversation_id`（扩展字段）| 请求中可传入以续接指定会话；响应中会返回，下次携带即可保持上下文 |

---

## 作为 Go 库使用

```go
import sentinel "sentinel-go"

client := sentinel.NewClient(sentinel.Config{
    BearerToken: "eyJ...",
    Model:       "gpt-5-5",
})

// 非流式（等待完整回复）
result, err := client.Chat(sentinel.ChatOptions{Text: "你好！"})
fmt.Println(result.Text)

// 流式（实时打印增量）
result, err := client.ChatStream(sentinel.ChatOptions{Text: "讲个故事"}, func(delta string) {
    fmt.Print(delta)
})

// 多轮对话（自动衔接，无需手动维护 ID）
client.Chat(sentinel.ChatOptions{Text: "我叫张三"})
result, _ = client.Chat(sentinel.ChatOptions{Text: "我叫什么名字？"}) // → 张三

// 重置会话（开启新对话）
client.ResetSession()

// 切换模型
client.SetModel("gpt-5-4")

// 禁用自动图片下载（由调用方异步处理）
client.SetDisableAutoImage(true)
```

### Config 字段

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `BearerToken` | string | ✅ | ChatGPT JWT Token |
| `CookieString` | string | ❌ | 浏览器 Cookie（可选，增强兼容性） |
| `Model` | string | ❌ | 模型名，默认 `gpt-5-5` |
| `DeviceID` | string | ❌ | 设备 ID，留空自动生成 UUID |
| `BuildHash` | string | ❌ | 客户端构建 Hash |
| `BuildNumber` | string | ❌ | 客户端构建号 |
| `UserAgent` | string | ❌ | User-Agent，默认模拟 Edge 146 |
| `Language` | string | ❌ | 语言，默认 `zh-CN` |
| `ImageDir` | string | ❌ | 图片下载目录，默认 `images/` |
| `TempMode` | bool | ❌ | 临时模式，默认 `false` |

### ChatResult 字段

| 字段 | 说明 |
|------|------|
| `Text` | 助手完整回复文本 |
| `ConversationID` | 对话 ID |
| `LastAssistantMsgID` | 最后一条助手消息 ID（多轮衔接用） |
| `ImageTaskID` | DALL-E 图片任务触发标志 |
| `ImageFileID` | 图片文件 ID（从 WebSocket asset_pointer 直接提取） |
| `ImagePath` | 已下载图片的本地路径 |

---

## 认证流程

每次发送消息前自动完成以下步骤：

```
1. POST /backend-api/f/conversation/prepare
       → 获取 conduit_token

2. POST /backend-api/sentinel/chat-requirements/prepare
       → 获取 PoW 挑战（seed + difficulty）

3. 本地 SHA3-512 暴力求解 Proof-of-Work Token
       → RequirementsToken (前缀 gAAAAAC) + ProofToken (前缀 gAAAAAB)

4. POST /backend-api/sentinel/chat-requirements/finalize
       → 获取 sentinel_token

5. GET  /backend-api/celsius/ws/user
       → 获取 WebSocket URL，建立持久连接

6. POST /backend-api/f/conversation (SSE)
       → 初始 SSE 流，获取 stream_handoff / turn_exchange_id

7. WebSocket 订阅 conversation-turn-{id}
       → 接收流式文本 delta（文字回复 / 生图思考过程 / 图片 asset_pointer）
```

---

## 依赖

| 依赖 | 说明 |
|------|------|
| [imroc/req/v3](https://github.com/imroc/req) | HTTP 客户端，支持 Chrome TLS 指纹伪装 |
| [gorilla/websocket](https://github.com/gorilla/websocket) | WebSocket 客户端 |
| [gin-gonic/gin](https://github.com/gin-gonic/gin) | API 服务器 HTTP 框架 |
| [golang.org/x/crypto/sha3](https://pkg.go.dev/golang.org/x/crypto/sha3) | SHA3-512（PoW 求解） |

---

## 注意事项

- 本项目仅供学习与研究使用，请勿用于商业或违反 OpenAI 服务条款的场景
- Bearer Token 是个人凭证，请勿泄露，**不要将 `config.json` 提交到公开仓库**
- 建议在 `.gitignore` 中添加以下内容：

```gitignore
config.json
tokens.json
tokens.txt
images/
```

---

## License

MIT
