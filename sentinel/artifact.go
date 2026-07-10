package sentinel

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// ArtifactSignalType 来自 SSE/对话结构的产物信号（非关键词）。
type ArtifactSignalType string

const (
	SignalImageGenTaskID   ArtifactSignalType = "image_gen_task_id"
	SignalGhostrider       ArtifactSignalType = "ghostrider"
	SignalDalleTool        ArtifactSignalType = "dalle_tool"
	SignalImageAsset       ArtifactSignalType = "image_asset_pointer"
	SignalPythonTool       ArtifactSignalType = "python_tool"
	SignalCodeInterpreter  ArtifactSignalType = "code_interpreter_recipient"
	SignalExecutionOutput  ArtifactSignalType = "execution_output"
	SignalSandboxPath      ArtifactSignalType = "sandbox_path"
	SignalContentReference ArtifactSignalType = "content_reference"
	SignalToolInvokedMeta  ArtifactSignalType = "tool_invoked_metadata" // server_ste_metadata
	SignalTurnUseCase      ArtifactSignalType = "turn_use_case"           // server_ste_metadata.turn_use_case
	SignalFileSearch       ArtifactSignalType = "file_search"             // 上传文件识图，非 DALL·E 生图
)

// ArtifactSignal 单条可观测信号。
type ArtifactSignal struct {
	Type   ArtifactSignalType `json:"type"`
	Value  string             `json:"value,omitempty"`
	Source string             `json:"source,omitempty"` // sse / conversation
}

// SandboxArtifact Code Interpreter 沙箱产物（pdf/txt/图片等）。
type SandboxArtifact struct {
	MessageID   string `json:"message_id"`
	SandboxPath string `json:"sandbox_path"`
	FileName    string `json:"file_name"`
}

// ArtifactPlan 流结束后应执行的拉取/轮询动作（由信号推导，非关键词）。
type ArtifactPlan struct {
	PollImage         bool `json:"poll_image"`
	PollSandboxFiles  bool `json:"poll_sandbox_files"`
	HasUserAttachment bool `json:"has_user_attachment"`
}

type signalFlags struct {
	imageGenTask    bool
	ghostrider      bool
	dalleTool       bool
	imageAsset      bool
	fileSearch      bool
	turnUseCase     string
	pythonTool      bool
	codeInterpreter bool
	executionOutput bool
	sandboxPath     bool
	toolInvokedMeta bool
}

// 沙箱路径可含中文等非 ASCII 文件名（如 你好世界测试123.txt）
var sandboxFileRe = regexp.MustCompile(`/mnt/data/[^"'()\s<>]+\.[A-Za-z0-9]+`)

func isValidSandboxPath(p string) bool {
	if !strings.HasPrefix(p, "/mnt/data/") {
		return false
	}
	if strings.ContainsAny(p, `"'()<>`) {
		return false
	}
	base := path.Base(p)
	return base != "." && base != "/" && strings.Contains(base, ".")
}

// ExtractSignalsFromJSON 递归扫描 JSON，收集结构化产物信号。
func ExtractSignalsFromJSON(v interface{}) []ArtifactSignal {
	var out []ArtifactSignal
	walkSignals(v, "", &out)
	return dedupeSignals(out)
}

func walkSignals(v interface{}, ctx string, out *[]ArtifactSignal) {
	switch x := v.(type) {
	case map[string]interface{}:
		inspectMessageMap(x, out)
		for k, val := range x {
			walkSignals(val, k, out)
		}
	case []interface{}:
		for _, item := range x {
			walkSignals(item, ctx, out)
		}
	case string:
		for _, m := range sandboxFileRe.FindAllString(x, -1) {
			if isValidSandboxPath(m) {
				*out = append(*out, ArtifactSignal{Type: SignalSandboxPath, Value: m})
			}
		}
		// 生图 asset_pointer：官网新版用 sediment:// 前缀，旧版为 file-service://，两者都识别。
		if strings.HasPrefix(x, "sediment://") || strings.HasPrefix(x, "file-service://") {
			if fid := extractFileID(x); fid != "" {
				*out = append(*out, ArtifactSignal{Type: SignalImageAsset, Value: fid})
			}
		}
	}
}

func inspectMessageMap(m map[string]interface{}, out *[]ArtifactSignal) {
	if t, ok := m["type"].(string); ok && t == "server_ste_metadata" {
		if md, ok := m["metadata"].(map[string]interface{}); ok {
			if inv, ok := md["tool_invoked"].(bool); ok && inv {
				toolName, _ := md["tool_name"].(string)
				*out = append(*out, ArtifactSignal{Type: SignalToolInvokedMeta, Value: toolName})
			}
			if uc, ok := md["turn_use_case"].(string); ok && uc != "" {
				*out = append(*out, ArtifactSignal{Type: SignalTurnUseCase, Value: uc})
			}
		}
	}
	if meta, ok := m["metadata"].(map[string]interface{}); ok {
		if tid, ok := meta["image_gen_task_id"].(string); ok && tid != "" {
			*out = append(*out, ArtifactSignal{Type: SignalImageGenTaskID, Value: tid})
		}
		if _, ok := meta["ghostrider"]; ok {
			*out = append(*out, ArtifactSignal{Type: SignalGhostrider, Value: "1"})
		}
		if refs, ok := meta["content_references"].([]interface{}); ok && len(refs) > 0 {
			*out = append(*out, ArtifactSignal{Type: SignalContentReference, Value: "present"})
		}
		if agg, ok := meta["aggregate_result"].(map[string]interface{}); ok {
			if code, ok := agg["code"].(string); ok {
				for _, p := range sandboxFileRe.FindAllString(code, -1) {
					if isValidSandboxPath(p) {
						*out = append(*out, ArtifactSignal{Type: SignalSandboxPath, Value: p})
					}
				}
			}
		}
	}
	if author, ok := m["author"].(map[string]interface{}); ok {
		role, _ := author["role"].(string)
		name, _ := author["name"].(string)
		if role == "tool" {
			lower := strings.ToLower(name)
			if strings.Contains(lower, "dalle") || strings.Contains(lower, "image_gen") {
				*out = append(*out, ArtifactSignal{Type: SignalDalleTool, Value: name})
			}
			if name == "file_search" {
				*out = append(*out, ArtifactSignal{Type: SignalFileSearch, Value: name})
			}
			if name == "python" || strings.Contains(lower, "canmore") {
				*out = append(*out, ArtifactSignal{Type: SignalPythonTool, Value: name})
			}
		}
	}
	if recipient, ok := m["recipient"].(string); ok && recipient == "code_interpreter" {
		*out = append(*out, ArtifactSignal{Type: SignalCodeInterpreter, Value: recipient})
	}
	if content, ok := m["content"].(map[string]interface{}); ok {
		ct, _ := content["content_type"].(string)
		if ct == "execution_output" || ct == "code" {
			*out = append(*out, ArtifactSignal{Type: SignalExecutionOutput, Value: ct})
		}
		if ct == "image_asset_pointer" {
			if parts, ok := content["parts"].([]interface{}); ok {
				for _, p := range parts {
					if pm, ok := p.(map[string]interface{}); ok {
						if ap, ok := pm["asset_pointer"].(string); ok && ap != "" {
							*out = append(*out, ArtifactSignal{Type: SignalImageAsset, Value: ap})
						}
					}
				}
			}
		}
	}
}

func dedupeSignals(in []ArtifactSignal) []ArtifactSignal {
	seen := make(map[string]bool, len(in))
	out := make([]ArtifactSignal, 0, len(in))
	for _, s := range in {
		key := string(s.Type) + "\x00" + s.Value
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

func MergeSignals(a, b []ArtifactSignal) []ArtifactSignal {
	return dedupeSignals(append(append([]ArtifactSignal{}, a...), b...))
}

func summarizeFlags(signals []ArtifactSignal) signalFlags {
	var f signalFlags
	for _, s := range signals {
		switch s.Type {
		case SignalImageGenTaskID:
			f.imageGenTask = true
		case SignalGhostrider:
			f.ghostrider = true
		case SignalDalleTool:
			f.dalleTool = true
		case SignalImageAsset:
			f.imageAsset = true
		case SignalFileSearch:
			f.fileSearch = true
		case SignalTurnUseCase:
			f.turnUseCase = s.Value
		case SignalPythonTool:
			f.pythonTool = true
		case SignalCodeInterpreter:
			f.codeInterpreter = true
		case SignalExecutionOutput:
			f.executionOutput = true
		case SignalSandboxPath:
			f.sandboxPath = true
		case SignalToolInvokedMeta:
			f.toolInvokedMeta = true
		}
	}
	return f
}

// IsGeneratedImageTurn 是否为 DALL·E / picture_v2 生图（区别于上传文件识图里的 sediment 引用）。
func IsGeneratedImageTurn(signals []ArtifactSignal, opts ChatOptions) bool {
	if opts.ForcePictureV2 {
		return true
	}
	f := summarizeFlags(signals)
	if f.fileSearch && !f.imageGenTask && !f.ghostrider && !f.dalleTool {
		return false
	}
	if f.turnUseCase == "multimodal" && len(opts.Images) > 0 && !f.imageGenTask && !f.ghostrider {
		return false
	}
	if f.turnUseCase == "image gen" {
		return true
	}
	for _, s := range signals {
		if s.Type == SignalToolInvokedMeta {
			lower := strings.ToLower(s.Value)
			if strings.Contains(lower, "imagegen") {
				return true
			}
		}
	}
	return f.imageGenTask || f.ghostrider || f.dalleTool
}

// SandboxArtifactsFromSignals 从已观测信号组装沙箱产物（无需再查 conversation API）。
func SandboxArtifactsFromSignals(signals []ArtifactSignal, messageID string) []SandboxArtifact {
	seen := make(map[string]bool)
	var arts []SandboxArtifact
	for _, s := range signals {
		if s.Type != SignalSandboxPath || s.Value == "" || !isValidSandboxPath(s.Value) || seen[s.Value] {
			continue
		}
		seen[s.Value] = true
		arts = append(arts, SandboxArtifact{
			MessageID:   messageID,
			SandboxPath: s.Value,
			FileName:    path.Base(s.Value),
		})
	}
	return arts
}

// ImageFileIDsFromSignals 从 SSE 信号提取图片 file_id（sediment://）。
func ImageFileIDsFromSignals(signals []ArtifactSignal) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, s := range signals {
		if s.Type != SignalImageAsset || s.Value == "" {
			continue
		}
		fid := extractFileID(s.Value)
		if fid == "" || seen[fid] {
			continue
		}
		seen[fid] = true
		ids = append(ids, fid)
	}
	return ids
}

// MergeApplyAndEmitArtifacts 合并信号、更新产物列表，并将新产物流式推送给客户端。
func (c *Client) MergeApplyAndEmitArtifacts(result *ChatResult, opts ChatOptions) {
	if result == nil {
		return
	}
	prevS := len(result.SandboxArtifacts)
	ApplyArtifactsFromSignals(result, opts)
	if len(result.SandboxArtifacts) > prevS {
		c.EmitNewArtifacts(opts.Artifacts, result)
	}
}

// ApplyArtifactsFromSignals 用流式累积信号填充沙箱/图片产物（不请求 conversation API）。
func ApplyArtifactsFromSignals(result *ChatResult, opts ChatOptions) {
	if result == nil {
		return
	}
	computedExpect := IsGeneratedImageTurn(result.ArtifactSignals, opts)
	// 若已确认为生图轮次（sawImageGenTool=true 或已有 imageSlots），保留该状态，避免被信号重置覆盖
	if !computedExpect && (result.sawImageGenTool || len(result.imageSlots) > 0) {
		computedExpect = true
	}
	result.ExpectGeneratedImages = computedExpect

	if len(result.SandboxArtifacts) == 0 {
		if arts := SandboxArtifactsFromSignals(result.ArtifactSignals, result.LastAssistantMsgID); len(arts) > 0 {
			result.SandboxArtifacts = arts
			result.PDFArtifacts = filterPDFArtifacts(arts)
		}
	}
	if !result.ExpectGeneratedImages {
		result.ImageFileIDs = nil
		result.ImageFileID = ""
		return
	}
	// 生图轮次：ImageFileIDs 仅由 WS conversation-update（含 dalle.gen_id）填充，
	// 勿把用户上传的 sediment 参考图写入，否则会误触发 4s idle 提前结束 WS。
	if result.ImageFileID == "" && len(result.ImageFileIDs) > 0 {
		result.ImageFileID = result.ImageFileIDs[0]
	}
}

// BuildArtifactPlan 根据 SSE 信号生成分析用计划（不触发 conversation 轮询）。
func BuildArtifactPlan(signals []ArtifactSignal, opts ChatOptions, imageTaskID string) ArtifactPlan {
	f := summarizeFlags(signals)
	plan := ArtifactPlan{HasUserAttachment: len(opts.Images) > 0}

	if imageTaskID != "" || f.imageGenTask || f.ghostrider || f.dalleTool {
		plan.PollImage = true
	}

	// 沙箱/图片产物均从 SSE 信号解析，不轮询 GET conversation。
	plan.PollSandboxFiles = false

	return plan
}

// AnalyzeSignals 生成可读分析报告（供 stream-capture 与调试）。
func AnalyzeSignals(name string, signals []ArtifactSignal, plan ArtifactPlan) map[string]interface{} {
	byType := make(map[string][]string)
	for _, s := range signals {
		byType[string(s.Type)] = append(byType[string(s.Type)], s.Value)
	}
	return map[string]interface{}{
		"case":               name,
		"signal_count":       len(signals),
		"signals_by_type":    byType,
		"plan":               plan,
		"analyzed_at":        time.Now().Format(time.RFC3339),
	}
}

func extractSandboxPathsFromValue(v interface{}) []string {
	var paths []string
	var walk func(interface{})
	walk = func(node interface{}) {
		switch x := node.(type) {
		case string:
			for _, m := range sandboxFileRe.FindAllString(x, -1) {
				paths = append(paths, m)
			}
		case map[string]interface{}:
			for _, val := range x {
				walk(val)
			}
		case []interface{}:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(v)
	return paths
}

// FetchConversationRaw 拉取完整对话 JSON。
func (c *Client) FetchConversationRaw(conversationID string) ([]byte, error) {
	apiPath := "/backend-api/conversation/" + conversationID
	resp, err := c.httpClient.R().
		SetHeaders(map[string]string{
			"x-openai-target-path":  apiPath,
			"x-openai-target-route": "/backend-api/conversation/{conversation_id}",
		}).
		Get(apiPath)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("conversation %d: %s", resp.StatusCode, truncateStr(resp.String(), 300))
	}
	return resp.Bytes(), nil
}

// ExtractSignalsFromConversation 从对话 mapping 提取信号。
func ExtractSignalsFromConversation(convJSON []byte) []ArtifactSignal {
	var conv map[string]interface{}
	if err := json.Unmarshal(convJSON, &conv); err != nil {
		return nil
	}
	signals := ExtractSignalsFromJSON(conv)
	for _, p := range extractSandboxPathsFromConversation(conv) {
		signals = append(signals, ArtifactSignal{Type: SignalSandboxPath, Value: p, Source: "conversation"})
	}
	return dedupeSignals(signals)
}

func extractSandboxPathsFromConversation(conv map[string]interface{}) []string {
	mapping, _ := conv["mapping"].(map[string]interface{})
	var all []string
	for _, nodeRaw := range mapping {
		node, ok := nodeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		all = append(all, extractSandboxPathsFromValue(node)...)
	}
	return all
}

// fetchSandboxArtifactsFromConversation 从对话 mapping 提取沙箱文件（仅 stream-capture/调试；正常聊天用 SSE 信号）。
func (c *Client) fetchSandboxArtifactsFromConversation(conversationID string) ([]SandboxArtifact, string, error) {
	raw, err := c.FetchConversationRaw(conversationID)
	if err != nil {
		return nil, "", err
	}
	var conv map[string]interface{}
	if err := json.Unmarshal(raw, &conv); err != nil {
		return nil, "", err
	}
	currentNode, _ := conv["current_node"].(string)
	mapping, _ := conv["mapping"].(map[string]interface{})

	type pathInfo struct {
		path      string
		messageID string
	}
	seen := make(map[string]bool)
	var paths []pathInfo

	for nodeID, nodeRaw := range mapping {
		node, ok := nodeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := node["message"].(map[string]interface{})
		if !ok {
			continue
		}
		msgID, _ := msg["id"].(string)
		if msgID == "" {
			msgID = nodeID
		}
		for _, p := range extractSandboxPathsFromValue(msg) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, pathInfo{path: p, messageID: msgID})
			}
		}
	}

	if len(paths) == 0 {
		return nil, currentNode, fmt.Errorf("对话中未找到沙箱文件")
	}

	ownerMsgID := findSandboxOwnerMessageID(mapping)
	if ownerMsgID == "" {
		ownerMsgID = currentNode
	}

	var artifacts []SandboxArtifact
	for _, pi := range paths {
		artifacts = append(artifacts, SandboxArtifact{
			MessageID:   ownerMsgID,
			SandboxPath: pi.path,
			FileName:    path.Base(pi.path),
		})
	}
	return artifacts, ownerMsgID, nil
}

func findSandboxOwnerMessageID(mapping map[string]interface{}) string {
	for _, nodeRaw := range mapping {
		node, ok := nodeRaw.(map[string]interface{})
		if !ok {
			continue
		}
		msg, ok := node["message"].(map[string]interface{})
		if !ok {
			continue
		}
		author, _ := msg["author"].(map[string]interface{})
		role, _ := author["role"].(string)
		if role != "assistant" {
			continue
		}
		meta, _ := msg["metadata"].(map[string]interface{})
		if refs, ok := meta["content_references"].([]interface{}); ok && len(refs) > 0 {
			if id, ok := msg["id"].(string); ok {
				return id
			}
		}
	}
	return ""
}
