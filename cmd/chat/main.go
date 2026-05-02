package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	sentinel "sentinel-go"
)

var accessTokenRe = regexp.MustCompile(`"accessToken"\s*:\s*"([^"]+)"`)

// extractToken 从原始字符串中提取 Bearer Token。
// 支持两种格式：
//  1. 直接粘贴 chatgpt.com/api/auth/session 的完整 JSON → 自动提取 accessToken
//  2. 直接粘贴 JWT 字符串 → 原样返回
func extractToken(raw string) string {
	raw = strings.TrimSpace(raw)
	if m := accessTokenRe.FindStringSubmatch(raw); len(m) == 2 {
		return m[1]
	}
	return raw
}

type configFile struct {
	BearerToken  string `json:"bearerToken"`
	CookieString string `json:"cookieString"`
}

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	model := flag.String("model", "gpt-5-5-thinking", "模型名称")
	temp := flag.Bool("temp", false, "临时模式（不保存历史）")
	flag.Parse()

	data, err := os.ReadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取配置文件失败: %v\n", err)
		os.Exit(1)
	}

	var cf configFile
	if err := json.Unmarshal(data, &cf); err != nil {
		fmt.Fprintf(os.Stderr, "解析配置文件失败: %v\n", err)
		os.Exit(1)
	}

	token := extractToken(cf.BearerToken)
	if token == "" || token == "REPLACE_WITH_JWT" {
		fmt.Fprintln(os.Stderr, "未配置凭证，请编辑 config.json")
		os.Exit(1)
	}

	client := sentinel.NewClient(sentinel.Config{
		BearerToken:  token,
		CookieString: cf.CookieString,
		Model:        *model,
		TempMode:     *temp,
	})

	args := flag.Args()
	if len(args) > 0 {
		userMsg := strings.Join(args, " ")
		fmt.Printf("\nYou: %s\n\nChatGPT:\n\n", userMsg)
		_, err := client.ChatStream(sentinel.ChatOptions{Text: userMsg}, func(delta string) {
			fmt.Print(delta)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\n[错误] %v\n", err)
			os.Exit(1)
		}
		fmt.Println()
		return
	}

	startRepl(client)
}

func startRepl(client *sentinel.Client) {
	reader := bufio.NewReader(os.Stdin)

	info := client.GetSessionInfo()
	fmt.Println("=== ChatGPT 多轮对话 (Go) ===")
	fmt.Printf("模型: %s | 临时模式: %s\n", info.Model, boolCN(info.TempMode))
	fmt.Println("命令: /new(新对话) /model <name>(切换模型) /temp(切换临时模式) /info(对话信息) /exit(退出)")
	fmt.Println()

	for {
		fmt.Print("You> ")
		input, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		switch {
		case input == "/exit" || input == "/quit":
			fmt.Println("再见！")
			return

		case input == "/new":
			client.ResetSession()
			fmt.Print("[ok] 已新建对话，上下文已重置\n\n")

		case strings.HasPrefix(input, "/model"):
			parts := strings.Fields(input)
			if len(parts) > 1 {
				client.SetModel(parts[1])
				fmt.Printf("[ok] 模型已切换为: %s\n\n", parts[1])
			} else {
				fmt.Printf("[当前模型] %s\n", client.GetModel())
				fmt.Print("  可选: gpt-5-5-thinking, gpt-4o, gpt-4o-mini, o4-mini-high\n\n")
			}

		case input == "/temp":
			info := client.GetSessionInfo()
			client.SetTempMode(!info.TempMode)
			newInfo := client.GetSessionInfo()
			if newInfo.TempMode {
				fmt.Print("[ok] 临时模式: 开 (不保存历史/不更新记忆)\n\n")
			} else {
				fmt.Print("[ok] 临时模式: 关 (正常保存)\n\n")
			}

		case input == "/info":
			info := client.GetSessionInfo()
			cid := info.ConversationID
			if cid == "" {
				cid = "(无，新对话)"
			}
			fmt.Printf("  conversation_id  : %s\n", cid)
			fmt.Printf("  parent_message_id: %s\n", info.ParentMessageID)
			fmt.Printf("  model            : %s\n", info.Model)
			fmt.Printf("  temp_mode        : %s\n", boolCN(info.TempMode))
			fmt.Printf("  turn             : %d\n\n", info.TurnCount)

		default:
			fmt.Print("\nChatGPT:\n\n")
			_, err := client.ChatStream(sentinel.ChatOptions{Text: input}, func(delta string) {
				fmt.Print(delta)
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "\n[错误] %v\n\n", err)
			} else {
				fmt.Println()
			}
		}
	}
}

func boolCN(b bool) string {
	if b {
		return "开"
	}
	return "关"
}
