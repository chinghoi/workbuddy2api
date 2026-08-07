package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInjectBrowserPluginRouting(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "hy3",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "developer",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": "- browser:control-in-app-browser: Control the in-app Browser. (file: /Users/test/.codex/plugins/browser/skills/control-in-app-browser/SKILL.md)",
				}},
			},
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": "测试 [@浏览器](plugin://browser@openai-bundled) 打开 baidu.com 并搜索 DeepSeek",
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	routed := injectBrowserPluginRouting(body)
	var request map[string]any
	if err := json.Unmarshal(routed, &request); err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("expected one injected item, got %d", len(input))
	}
	item, _ := input[1].(map[string]any)
	if role, _ := item["role"].(string); role != "developer" {
		t.Fatalf("expected injected developer item, got role %q", role)
	}
	text := bridgeExtractContent(item["content"])
	for _, expected := range []string{
		browserRoutingMarker,
		"/Users/test/.codex/plugins/browser/skills/control-in-app-browser/SKILL.md",
		"mcp__node_repl__js",
		"Do not call request_plugin_install",
		"Do not substitute fetch",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("routing instruction missing %q: %s", expected, text)
		}
	}
}

func TestInjectBrowserPluginRoutingLeavesOtherRequestsUntouched(t *testing.T) {
	body := []byte(`{"model":"hy3","input":"hello"}`)
	if got := injectBrowserPluginRouting(body); string(got) != string(body) {
		t.Fatalf("unrelated request changed: %s", got)
	}
}
