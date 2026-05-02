package sentinel

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"time"
)

// GenerateUUID 生成 v4 UUID
func GenerateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func perfNowMs(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000.0
}



func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

func runeSlice(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) > maxRunes {
		r = r[:maxRunes]
	}
	return string(r)
}

func orDefault(val, def string) string {
	if val != "" {
		return val
	}
	return def
}

// getNestedString 从嵌套 map 中按路径取 string 值
func getNestedString(m map[string]interface{}, keys ...string) string {
	current := m
	for i, key := range keys {
		if i == len(keys)-1 {
			s, _ := current[key].(string)
			return s
		}
		next, ok := current[key].(map[string]interface{})
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

// getFirstStringPart 从 message 的 content.parts[0] 取字符串
func getFirstStringPart(msg map[string]interface{}) string {
	content, ok := msg["content"].(map[string]interface{})
	if !ok {
		return ""
	}
	parts, ok := content["parts"].([]interface{})
	if !ok || len(parts) == 0 {
		return ""
	}
	s, _ := parts[0].(string)
	return s
}

var fileIDRegexp = regexp.MustCompile(`file_[a-f0-9]+`)

func extractFileID(pointer string) string {
	if pointer == "" {
		return ""
	}
	return fileIDRegexp.FindString(pointer)
}
