package sentinel

import (
	"encoding/json"
	"testing"
)

func TestExtractFinalAssistantTextIgnoresOpenTurn(t *testing.T) {
	raw := []byte(`{
		"mapping": {
			"a": {
				"message": {
					"id": "m1",
					"author": {"role": "assistant"},
					"end_turn": false,
					"create_time": 1,
					"content": {"content_type": "text", "parts": ["开场白，我这就分析…"]}
				}
			},
			"b": {
				"message": {
					"id": "m2",
					"author": {"role": "assistant"},
					"end_turn": true,
					"create_time": 2,
					"content": {"content_type": "text", "parts": ["{\"product_name\":\"LED bulb\",\"model\":\"HY24\"}"]}
				}
			}
		}
	}`)
	var conv map[string]interface{}
	if err := json.Unmarshal(raw, &conv); err != nil {
		t.Fatal(err)
	}
	txt, mid := extractFinalAssistantText(conv)
	if mid != "m2" {
		t.Fatalf("want msg m2, got %q", mid)
	}
	if txt == "" || txt[0] != '{' {
		t.Fatalf("want final JSON body, got %q", txt)
	}
}
