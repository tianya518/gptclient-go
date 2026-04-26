# sentinel-go

> 用 Go 语言逆向实现的 ChatGPT Web 端非官方客户端库，无需 OpenAI API Key，直接使用浏览器 Bearer Token 与 ChatGPT 对话。

---

## 特性

- ✅ 完整实现 ChatGPT Web 端 Sentinel 认证流程（conduit token + PoW + sentinel token）
- ✅ 支持 SSE 流式输出（实时回调增量文本）
- ✅ 支持多轮对话（自动维护 conversation_id / parent_message_id）
- ✅ 支持 DALL-E 图片生成并自动下载到本地
- ✅ 支持临时模式（不保存对话历史 / 不更新记忆）
- ✅ 浏览器指纹伪装（TLS 指纹 + Chrome UA + sec-ch-ua 全套 Headers）
- ✅ 开箱即用的交互式 CLI（REPL）

---

## 快速开始

### 1. 克隆项目

```bash
git clone https://github.com/yourname/sentinel-go.git
cd sentinel-go
```

### 2. 获取 Bearer Token

1. 打开浏览器，登录 [https://chatgpt.com](https://chatgpt.com)
2. 按 `F12` 打开开发者工具 → Network 面板
3. 随便发一条消息，过滤请求找到 `/backend-api/conversation`
4. 在请求 Headers 中找到 `Authorization: Bearer eyJ...`，复制 `Bearer ` 后面的完整 JWT

> ⚠️ Token 有效期约 **10 天**，过期后需重新获取。

### 3. 配置凭证

编辑项目根目录的 `config.json`：

```json
{
  "bearerToken": "eyJhbGciOi...（你的 JWT Token）",
  "cookieString": ""
}
```

### 4. 运行

```bash
# 交互式多轮对话（REPL 模式）
go run ./cmd/chat/

# 单次问答
go run ./cmd/chat/ "你好，介绍一下自己"

# 指定模型
go run ./cmd/chat/ -model gpt-4o-mini "帮我写一段 Go 代码"

# 临时模式（不保存历史）
go run ./cmd/chat/ -temp
```

---

## CLI 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-config` | `config.json` | 配置文件路径 |
| `-model` | `gpt-5-4-thinking` | 使用的模型名称 |
| `-temp` | `false` | 开启临时模式（不保存对话历史） |

---

## REPL 交互命令

进入交互模式后，可使用以下内置命令：

| 命令 | 说明 |
|------|------|
| `/new` | 开启新对话，清空上下文 |
| `/model <name>` | 切换模型（不传参数则显示当前模型及可选列表） |
| `/temp` | 切换临时模式开/关 |
| `/info` | 查看当前会话详情（conversation_id、model、轮次等） |
| `/exit` 或 `/quit` | 退出程序 |

**可选模型参考：**

```
gpt-4o
gpt-4o-mini
gpt-5-5-thinking
o4-mini-high
```

---

## 作为库使用

```go
import sentinel "sentinel-go"

client := sentinel.NewClient(sentinel.Config{
    BearerToken: "eyJ...",
    Model:       "gpt-4o",
    TempMode:    false,
})

// 非流式（等待完整回复）
result, err := client.Chat("你好！")
fmt.Println(result.Text)

// 流式（实时打印增量）
result, err := client.ChatStream("讲个故事", func(delta string) {
    fmt.Print(delta)
})

// 多轮对话（无需手动维护 ID，自动衔接）
client.Chat("我叫张三")
result, _ := client.Chat("我叫什么名字？") // → 张三

// 重置会话（开启新对话）
client.ResetSession()

// 切换模型
client.SetModel("gpt-4o-mini")
```

### Config 字段说明

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `BearerToken` | string | ✅ | ChatGPT JWT Token |
| `CookieString` | string | ❌ | 浏览器 Cookie（可选，增强兼容性） |
| `Model` | string | ❌ | 模型名，默认 `gpt-5-4-thinking` |
| `DeviceID` | string | ❌ | 设备 ID，留空自动生成 UUID |
| `BuildHash` | string | ❌ | 客户端构建 Hash |
| `BuildNumber` | string | ❌ | 客户端构建号 |
| `UserAgent` | string | ❌ | User-Agent，默认模拟 Edge 146 |
| `Language` | string | ❌ | 语言，默认 `zh-CN` |
| `ImageDir` | string | ❌ | 图片下载目录，默认 `images/` |
| `TempMode` | bool | ❌ | 临时模式，默认 `false` |

### ChatResult 字段说明

| 字段 | 说明 |
|------|------|
| `Text` | 助手完整回复文本 |
| `ConversationID` | 对话 ID |
| `LastAssistantMsgID` | 最后一条助手消息 ID（多轮衔接用） |
| `ImageTaskID` | DALL-E 图片任务 ID（如有） |
| `ImagePath` | 已下载图片的本地路径（如有） |

---

## 项目结构

```
sentinel-go/
├── types.go          # 公开类型定义
├── client.go         # Client 核心结构体 & HTTP 初始化
├── auth.go           # Sentinel 三步认证流程
├── chat.go           # 对话主流程 & SSE 事件解析
├── image.go          # DALL-E 图片轮询 & 下载
├── utils.go          # UUID、FNV Hash、浏览器指纹构造
├── config.json       # 本地凭证配置（不要提交到 Git）
├── go.mod
└── cmd/
    └── chat/
        └── main.go   # CLI 入口
```

---

## 认证流程说明

每次发送消息前，会依次完成以下步骤：

```
1. POST /conversation/prepare      → 获取 conduit_token
2. POST /sentinel/chat-requirements/prepare → 获取 PoW 挑战
3. （若需要 PoW）暴力枚举 FNV-1a Hash 直到满足难度前缀
4. POST /sentinel/chat-requirements/finalize → 获取 sentinel_token
5. POST /backend-api/f/conversation (SSE)  → 流式获取回复
```

---

## 依赖

| 依赖 | 说明 |
|------|------|
| [imroc/req/v3](https://github.com/imroc/req) | HTTP 客户端，支持 Chrome TLS 指纹伪装 |
| [refraction-networking/utls](https://github.com/refraction-networking/utls) | TLS 指纹库（间接依赖） |
| [quic-go/quic-go](https://github.com/quic-go/quic-go) | HTTP/3 支持（间接依赖） |

---

## 注意事项

- 本项目仅供学习与研究使用，请勿用于商业或违反 OpenAI 服务条款的场景
- Bearer Token 是个人凭证，请勿泄露，**不要将 `config.json` 提交到公开仓库**
- 建议在 `.gitignore` 中添加 `config.json`

```gitignore
config.json
images/
```

---

## License

MIT
