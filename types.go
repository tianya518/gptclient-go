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
	ProxyURL     string // 可选：HTTP/SOCKS5 代理，如 socks5://127.0.0.1:10816
}

// ThinkStep 思考过程中的一个步骤（默认由 SSE thoughts 填充；可选 fetchTextdocs）
type ThinkStep struct {
	Summary string // 步骤标题（简短）
	Content string // 详细推理内容
}

// ChatResult 单轮对话结果
type ChatResult struct {
	Text               string        // 助手回复的完整文本
	ThinkingText       string        // 思考过程文本（analysis channel，用于追踪增量）
	ThinkSteps         []ThinkStep   // 思考步骤列表（含 summary + content 详细内容）
	deltaChannel       string        // 内部：当前 delta 消息的 channel（analysis / final / ""）
	sawAnalysisChannel bool          // 本 turn 是否出现过 analysis（用于未标 channel 的 patch 分流）
	assistantFinalText string        // final channel 正文（优先作为 result.Text）
	emittedBodyLen     int           // 已通过 handler 下发的正文字节数（防 WS catchup 重放）
	bodyStreamFromSSE  bool          // HTTP SSE 已输出过正文，WS catchups 应跳过
	seenThoughtKeys    map[string]bool // 内部：已推送过的 thought key（summary，去重用）
	ConversationID     string        // 对话 ID
	LastAssistantMsgID string        // 最后一条助手消息 ID（用于多轮衔接）
	ImageTaskID        string        // DALL-E 图片任务触发标志（如有）
	ImageFileID        string        // 首张图片文件 ID（兼容旧逻辑，等同于 ImageFileIDs[0]）
	ImageFileIDs           []string // 所有生成图片的文件 ID 列表（多图场景）
	ExpectGeneratedImages  bool     // 本轮为 DALL·E/picture_v2 生图（非上传文件识图里的 sediment 引用）
	TurnExchangeID           string   // 本轮 turn_exchange_id（SSE/WS message.metadata）
	ImagePath              string   // 已下载图片本地路径（如有）
	DalleStarted       bool              // 标记是否已输出正在画图的提示
	ArtifactSignals    []ArtifactSignal  // 流式/对话中观测到的产物信号（用于通用轮询判断）
	SandboxArtifacts   []SandboxArtifact // Code Interpreter 沙箱产物（pdf/txt/等）
	PDFArtifacts       []PDFArtifact     // 兼容：.pdf 子集，与 SandboxArtifacts 同步填充
	emittedArtifacts   map[string]bool              // 已推送的产物键（防重复）
	lastImageAddedAt       int64  // 纳秒，最近一次 DALL·E file_id 更新时间
	lastImageGenActivityAt int64  // 纳秒，生图相关任意活动（图/async-task/turn WS）— idle 据此判断
	imageSlots             map[string]*GeneratedImageSlot
	imageAsyncTaskActive      bool // 仍有进行中的 image_gen async task
	imageAsyncTaskPending     int  // async-task-start 与 complete/end 计数
	imageGenAsyncCompleteSeen    bool // 已收到 async-task-complete/end
	imageGenConvAsyncStatusDone  bool // set-conversation-async-status（如 status=4）
	imageGenConvStatusAt         int64 // 收到 conv async status=4 的时间（纳秒）
	imageGenTurnDone             bool // turn topic WS 流已 [DONE]（仅诊断，不单独作为结束条件）
	userReferenceFileIDs         map[string]bool // 用户上传参考图 file_id（picture_v2 时勿当作生图产出）
	pictureV2Path                bool            // 请求级 picture_v2 / ForcePictureV2
	sawImageGenTool              bool            // 本轮出现过 image_gen / dalle 工具
	imageGenConvFetched          bool            // 已尝试过 conversation API 回退拉取
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
