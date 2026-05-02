package sentinel

// Config 客户端配置
type Config struct {
	BearerToken  string // 必需：ChatGPT Bearer Token (JWT)
	CookieString string // 可选：Cookie 字符串
	Model        string // 可选：默认 "gpt-5-5-thinking"
	DeviceID     string // 可选：设备 ID，留空自动生成 UUID
	BuildHash    string // 可选：客户端构建哈希
	BuildNumber  string // 可选：客户端构建号
	UserAgent    string // 可选：User-Agent 字符串
	Language     string // 可选：语言，默认 "zh-CN"
	ImageDir     string // 可选：图片保存目录，默认 "images"
	TempMode     bool   // 可选：临时模式（不保存对话历史）
}

// ChatResult 单轮对话结果
type ChatResult struct {
	Text               string // 助手回复的完整文本
	ConversationID     string // 对话 ID
	LastAssistantMsgID string // 最后一条助手消息 ID（用于多轮衔接）
	ImageTaskID        string // DALL-E 图片任务触发标志（如有）
	ImageFileID        string // 图片文件 ID（从 asset_pointer 提取，如 file_xxx）
	ImagePath          string // 已下载图片本地路径（如有）
	DalleStarted       bool   // 标记是否已输出正在画图的提示
}

// SessionInfo 当前会话状态快照
type SessionInfo struct {
	ConversationID  string
	ParentMessageID string
	Model           string
	TempMode        bool
	TurnCount       int
}

// StreamHandler 流式回调，每次收到文本增量时被调用
type StreamHandler func(delta string)

// LogFunc 日志输出函数签名
type LogFunc func(format string, args ...interface{})
