// stream-capture 运行三种产物场景，将 SSE 流与对话 JSON 写入本地供分析。
//
// 用法:
//
//	go run ./cmd/stream-capture/ -config config.json
//	go run ./cmd/stream-capture/ -config config.json -case image
//
// 输出目录: testdata/stream-captures/<timestamp>-<case>/
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sentinel "sentinel-go/sentinel"
)

type captureCase struct {
	Name           string
	Prompt         string
	Model          string
	ForcePictureV2 bool
}

var defaultCases = []captureCase{
	{
		Name:           "image",
		Prompt:         "画一只在沙发上的橘猫，卡通风格，单张图即可",
		Model:          "gpt-4o",
		ForcePictureV2: true,
	},
	{
		Name:   "txt",
		Prompt: "请用 Code Interpreter 生成一个 txt 文件，文件内容必须是：你好世界测试123",
	},
	{
		Name:   "pdf",
		Prompt: "请用 Code Interpreter 生成一份 PDF，介绍小猫的品种和习性，约一页即可",
	},
}

func main() {
	configPath := flag.String("config", "", "config.json（bearerToken）")
	tokensPath := flag.String("tokens", "tokens.json", "tokens.json 路径（优先于 -config）")
	outRoot := flag.String("out", "testdata/stream-captures", "输出根目录")
	onlyCase := flag.String("case", "", "只运行指定 case: image|txt|pdf")
	flag.Parse()

	token, err := loadBearerToken(*configPath, *tokensPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cases := defaultCases
	if *onlyCase != "" {
		var filtered []captureCase
		for _, c := range cases {
			if c.Name == *onlyCase {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) == 0 {
			fmt.Fprintf(os.Stderr, "未知 case: %s\n", *onlyCase)
			os.Exit(1)
		}
		cases = filtered
	}

	stamp := time.Now().Format("20060102-150405")
	for _, tc := range cases {
		dir := filepath.Join(*outRoot, fmt.Sprintf("%s-%s", stamp, tc.Name))
		if err := os.MkdirAll(dir, 0755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("\n=== case %s → %s ===\n", tc.Name, dir)
		if err := runCase(token, tc, dir); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] 失败: %v\n", tc.Name, err)
		}
	}
	fmt.Println("\n完成。请查看各目录下 sse.ndjson / conversation.json / analysis.json")
}

func runCase(token string, tc captureCase, dir string) error {
	model := tc.Model
	if model == "" {
		model = "gpt-5-5-thinking"
	}

	rec, err := sentinel.NewStreamRecorder(filepath.Join(dir, "sse.ndjson"))
	if err != nil {
		return err
	}
	defer rec.Close()
	_ = rec.OpenWSLog(filepath.Join(dir, "ws.ndjson"))

	client := sentinel.NewClient(sentinel.Config{
		BearerToken: token,
		Model:       model,
		TempMode:    false, // 临时模式无法拉 conversation / 生图，抓包须用正式会话
	})
	client.StreamRecorder = rec
	client.ResetSession()

	opts := sentinel.ChatOptions{
		Text:           tc.Prompt,
		ForcePictureV2: tc.ForcePictureV2,
	}

	fmt.Printf("提示: %s\n", tc.Prompt)
	var streamLog strings.Builder
	result, err := client.ChatStream(opts, func(delta string) {
		streamLog.WriteString(delta)
		fmt.Print(delta)
	})
	fmt.Println()
	if err != nil {
		return err
	}

	_ = os.WriteFile(filepath.Join(dir, "assistant_text.txt"), []byte(result.Text), 0644)
	_ = os.WriteFile(filepath.Join(dir, "stream_stdout.txt"), []byte(streamLog.String()), 0644)

	chatResult, _ := json.MarshalIndent(result, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "chat_result.json"), chatResult, 0644)

	sseSignals := rec.AllSignals()
	plan := sentinel.BuildArtifactPlan(sseSignals, opts, result.ImageTaskID)
	plan = mergePlan(plan, sentinel.BuildArtifactPlan(result.ArtifactSignals, opts, result.ImageTaskID))

	var convSignals []sentinel.ArtifactSignal
	if result.ConversationID != "" {
		raw, err := client.FetchConversationRaw(result.ConversationID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "拉取 conversation 失败: %v\n", err)
		} else {
			_ = os.WriteFile(filepath.Join(dir, "conversation.json"), raw, 0644)
			convSignals = sentinel.ExtractSignalsFromConversation(raw)
		}
	}

	allSignals := sseSignals
	allSignals = sentinel.MergeSignals(allSignals, result.ArtifactSignals)
	allSignals = sentinel.MergeSignals(allSignals, convSignals)

	analysis := sentinel.AnalyzeSignals(tc.Name, allSignals, plan)
	analysis["conversation_id"] = result.ConversationID
	analysis["image_task_id"] = result.ImageTaskID
	analysis["image_file_ids"] = result.ImageFileIDs
	analysis["sandbox_artifacts"] = result.SandboxArtifacts
	analysis["sse_event_count"] = len(rec.Entries())

	ab, _ := json.MarshalIndent(analysis, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "analysis.json"), ab, 0644)

	summary := fmt.Sprintf(`case=%s
conversation_id=%s
sse_events=%d
signals=%d
plan_poll_image=%v
plan_poll_sandbox=%v
image_file_ids=%v
sandbox_files=%v
`,
		tc.Name, result.ConversationID, len(rec.Entries()), len(allSignals),
		plan.PollImage, plan.PollSandboxFiles,
		result.ImageFileIDs, sandboxNames(result.SandboxArtifacts),
	)
	_ = os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(summary), 0644)

	fmt.Print(summary)
	return nil
}

func mergePlan(a, b sentinel.ArtifactPlan) sentinel.ArtifactPlan {
	return sentinel.ArtifactPlan{
		PollImage:         a.PollImage || b.PollImage,
		PollSandboxFiles:  a.PollSandboxFiles || b.PollSandboxFiles,
		HasUserAttachment: a.HasUserAttachment || b.HasUserAttachment,
	}
}

func sandboxNames(arts []sentinel.SandboxArtifact) []string {
	names := make([]string, len(arts))
	for i, a := range arts {
		name := a.FileName
		if name == "" {
			name = a.SandboxPath
		}
		names[i] = name
	}
	return names
}

func loadBearerToken(configPath, tokensPath string) (string, error) {
	if tokensPath != "" {
		if at, err := loadFromTokensJSON(tokensPath); err == nil && at != "" {
			return at, nil
		} else if err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	if configPath == "" {
		configPath = "config.json"
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("读取凭证失败（-tokens 或 -config）: %w", err)
	}
	return extractAccessToken(string(data))
}

func loadFromTokensJSON(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var tf struct {
		Tokens []struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &tf); err != nil {
		return "", err
	}
	if len(tf.Tokens) == 0 || tf.Tokens[0].AccessToken == "" {
		return "", fmt.Errorf("tokens.json 无 access_token")
	}
	return tf.Tokens[0].AccessToken, nil
}

func extractAccessToken(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, `"accessToken"`) || strings.Contains(raw, `"access_token"`) {
		for _, key := range []string{`"access_token":"`, `"accessToken":"`} {
			if i := strings.Index(raw, key); i >= 0 {
				s := raw[i+len(key):]
				if j := strings.Index(s, `"`); j > 0 {
					return s[:j], nil
				}
			}
		}
	}
	if strings.HasPrefix(raw, "eyJ") {
		return raw, nil
	}
	return "", fmt.Errorf("未找到 access token")
}
