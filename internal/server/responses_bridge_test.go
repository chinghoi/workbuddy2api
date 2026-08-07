package server

import (
	"encoding/json"
	"strings"
	"testing"

	"workbuddy2api/internal/upstream"
)

func TestBridgeHoistsChatGPTDeveloperCustomTool(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"input":[
			{"role":"developer","tools":[{"type":"custom","name":"exec","description":"Run JavaScript code to orchestrate tools. no network access. file system. delete write execute ` + strings.Repeat("x", 3000) + `"}]},
			{"role":"user","content":"hello"}
		],
		"reasoning":{"effort":"low"},
		"parallel_tool_calls":true,
		"tool_choice":"auto"
	}`)
	bridge := bridgeResponsesRequest(body)
	if !bridge.Tools.IsCustom("exec") {
		t.Fatalf("exec metadata not custom: %#v", bridge.Tools)
	}
	var req map[string]any
	if err := json.Unmarshal(bridge.ChatBody, &req); err != nil {
		t.Fatal(err)
	}
	if req["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort=%v", req["reasoning_effort"])
	}
	messages := req["messages"].([]any)
	if len(messages) != 1 || messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("messages=%#v", messages)
	}
	tools := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "exec" {
		t.Fatalf("name=%v", fn["name"])
	}
	desc := fn["description"].(string)
	// The compacted description must stay bounded, must not leak the raw
	// multi-kilobyte schema, and must preserve the result-shape distinction that
	// programmatic MCP calls depend on.
	if len(desc) > 1200 || strings.Contains(desc, strings.Repeat("x", 64)) {
		t.Fatalf("description not compacted: %q", desc)
	}
	for _, expected := range []string{
		"exec_command uses r.output",
		"MCP methods (tools.mcp__*) return CallToolResult",
		"r?.content",
		"never assume r.output",
		"If a nested tool returns a string, use text(r)",
	} {
		if !strings.Contains(desc, expected) {
			t.Fatalf("exec description missing %q: %s", expected, desc)
		}
	}
	params := fn["parameters"].(map[string]any)
	if _, ok := params["properties"].(map[string]any)["input"]; !ok {
		t.Fatalf("params=%#v", params)
	}
}

func TestBridgeCustomToolRoundTripInputItems(t *testing.T) {
	body := []byte(`{
		"model":"glm-5.2",
		"input":[
			{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"text(\"ok\")"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":"done"}
		]
	}`)
	bridge := bridgeResponsesRequest(body)
	var req map[string]any
	if err := json.Unmarshal(bridge.ChatBody, &req); err != nil {
		t.Fatal(err)
	}
	messages := req["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages=%#v", messages)
	}
	assistant := messages[0].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)
	args := call["function"].(map[string]any)["arguments"].(string)
	if upstream.DecodeCustomToolInput(args) != `text("ok")` {
		t.Fatalf("args=%q", args)
	}
	tool := messages[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" || tool["content"] != "done" {
		t.Fatalf("tool=%#v", tool)
	}
}

func TestBridgeTopLevelToolOverridesEmbeddedDuplicate(t *testing.T) {
	body := []byte(`{
		"input":[{"role":"developer","tools":[{"type":"custom","name":"same","description":"embedded"}]}],
		"tools":[{"type":"function","name":"same","description":"top","parameters":{"type":"object","properties":{}}}]
	}`)
	bridge := bridgeResponsesRequest(body)
	if bridge.Tools.IsCustom("same") {
		t.Fatalf("top-level function did not override embedded custom")
	}
	var req map[string]any
	_ = json.Unmarshal(bridge.ChatBody, &req)
	tools := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["description"] != "top" {
		t.Fatalf("fn=%#v", fn)
	}
}
