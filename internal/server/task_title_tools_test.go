package server

import (
	"encoding/json"
	"testing"
)

func TestStripTaskTitleToolsRemovesAllToolSurfacesAndHistory(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "glm-5.2",
		"tools": []any{
			map[string]any{"type": "custom", "name": "exec"},
		},
		"functions": []any{
			map[string]any{"name": "legacy_exec"},
		},
		"tool_choice":         "auto",
		"function_call":       "auto",
		"parallel_tool_calls": false,
		"input": []any{
			map[string]any{
				"type": "additional_tools",
				"role": "developer",
				"tools": []any{
					map[string]any{"type": "custom", "name": "exec"},
					map[string]any{"type": "function", "name": "wait"},
				},
			},
			map[string]any{
				"type": "message",
				"role": "developer",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": "Generate only a title.",
				}},
			},
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": "You are a helpful assistant. You will be presented with a user prompt, and your job is to provide a short title for a task. User prompt: 生成一个 SVG 动画 HTML",
				}},
			},
			map[string]any{
				"type":    "custom_tool_call",
				"id":      "call_title_placeholder",
				"call_id": "call_title_placeholder",
				"name":    "exec",
				"input":   "title placeholder",
			},
			map[string]any{
				"type":    "custom_tool_call_output",
				"call_id": "call_title_placeholder",
				"output":  "placeholder",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stripped := stripTaskTitleTools(body)
	var request map[string]any
	if err := json.Unmarshal(stripped, &request); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tools", "functions", "tool_choice", "function_call", "parallel_tool_calls"} {
		if _, exists := request[key]; exists {
			t.Fatalf("top-level %s survived: %#v", key, request[key])
		}
	}

	input, _ := request["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("expected only developer and title prompt messages, got %d: %#v", len(input), input)
	}
	for index, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			t.Fatalf("input %d is not an object: %#v", index, rawItem)
		}
		if itemType, _ := item["type"].(string); itemType == "additional_tools" {
			t.Fatalf("additional_tools item survived: %#v", item)
		}
		for _, key := range []string{"tools", "functions", "tool_choice", "function_call", "parallel_tool_calls"} {
			if _, exists := item[key]; exists {
				t.Fatalf("embedded %s survived in input %d: %#v", key, index, item[key])
			}
		}
	}

	bridge := bridgeResponsesRequest(stripped)
	if len(bridge.Tools) != 0 {
		t.Fatalf("title bridge retained tool specs: %#v", bridge.Tools)
	}
	var chat map[string]any
	if err := json.Unmarshal(bridge.ChatBody, &chat); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tools", "functions", "tool_choice", "function_call", "parallel_tool_calls"} {
		if _, exists := chat[key]; exists {
			t.Fatalf("bridged title request retained %s: %#v", key, chat[key])
		}
	}
	messages, _ := chat["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("bridged title request has unexpected history: %#v", messages)
	}
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if role, _ := message["role"].(string); role == "tool" {
			t.Fatalf("bridged title request retained tool output: %#v", message)
		}
		if _, exists := message["tool_calls"]; exists {
			t.Fatalf("bridged title request retained tool calls: %#v", message)
		}
	}
}

func TestStripTaskTitleToolsLeavesNormalRequestsByteIdentical(t *testing.T) {
	body := []byte(`{"model":"hy3","tools":[{"type":"custom","name":"exec"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"生成一个 SVG 动画 HTML"}]}]}`)
	if got := stripTaskTitleTools(body); string(got) != string(body) {
		t.Fatalf("normal request changed: %s", got)
	}
}
