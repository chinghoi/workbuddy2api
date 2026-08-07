package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestGuardBrowserRetryLoopAddsSingleRecoveryInstruction(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", output: "SyntaxError: Identifier 'tab' has already been declared"},
	})

	guarded := guardBrowserRetryLoop(body)
	var request map[string]any
	if err := json.Unmarshal(guarded, &request); err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	if got := countBrowserRecoveryMessages(input); got != 1 {
		t.Fatalf("expected one recovery instruction, got %d", got)
	}
	text := browserRecoveryText(input)
	for _, expected := range []string{
		"Make at most one recovery tool call",
		"one nested async IIFE",
		"nodeRepl.write",
		"Identifier 'tab' has already been declared",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("recovery instruction missing %q: %s", expected, text)
		}
	}
	if !hasAdditionalToolsItem(input) {
		t.Fatal("single failure unexpectedly removed browser tools")
	}
	if len(bridgeResponsesRequest(guarded).Tools) == 0 {
		t.Fatal("single failure unexpectedly removed bridged tool specs")
	}
}

func TestGuardBrowserRetryLoopStopsAfterThreeFailuresAndCompactsHistory(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", output: "undefined\nundefined"},
		{id: "call_2", output: "SyntaxError: Identifier 'info' has already been declared"},
		{id: "call_3", output: "TypeError: allInnerTexts is not a function"},
	})

	guarded := guardBrowserRetryLoop(body)
	var request map[string]any
	if err := json.Unmarshal(guarded, &request); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tools", "functions", "tool_choice", "function_call", "parallel_tool_calls"} {
		if _, exists := request[key]; exists {
			t.Fatalf("retry circuit breaker retained top-level %s", key)
		}
	}
	input, _ := request["input"].([]any)
	if hasAdditionalToolsItem(input) {
		t.Fatal("retry circuit breaker retained additional_tools")
	}
	text := browserRecoveryText(input)
	if !strings.Contains(text, "failed 3 consecutive times") ||
		!strings.Contains(text, "Do not call any tool again") {
		t.Fatalf("missing circuit-breaker instruction: %s", text)
	}
	if len(bridgeResponsesRequest(guarded).Tools) != 0 {
		t.Fatal("retry circuit breaker still exposed bridged tools")
	}

	calls := map[string]map[string]any{}
	outputs := map[string]map[string]any{}
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		switch typ, _ := item["type"].(string); typ {
		case "custom_tool_call":
			calls[bridgeCallID(item)] = item
		case "custom_tool_call_output":
			outputs[bridgeCallID(item)] = item
		}
	}
	if calls["call_1"]["input"] != browserOmittedCallInput {
		t.Fatalf("oldest failed call was not compacted: %#v", calls["call_1"])
	}
	if outputs["call_1"]["output"] != browserOmittedToolOutput {
		t.Fatalf("oldest failed output was not compacted: %#v", outputs["call_1"])
	}
	if calls["call_2"]["input"] == browserOmittedCallInput || calls["call_3"]["input"] == browserOmittedCallInput {
		t.Fatal("recent failed calls should remain available for diagnosis")
	}
}

func TestGuardBrowserRetryLoopSuccessfulAttemptResetsRecovery(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", output: "[object Object]"},
		{id: "call_2", output: `{"url":"https://www.google.com/search?q=Linux","title":"Linux - Google Search"}`},
	})
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	input = append(input, browserGuardMessage("stale recovery"))
	request["input"] = input
	body, _ = json.Marshal(request)

	guarded := guardBrowserRetryLoop(body)
	var got map[string]any
	if err := json.Unmarshal(guarded, &got); err != nil {
		t.Fatal(err)
	}
	gotInput, _ := got["input"].([]any)
	if count := countBrowserRecoveryMessages(gotInput); count != 0 {
		t.Fatalf("successful browser attempt left %d recovery messages", count)
	}
	if !hasAdditionalToolsItem(gotInput) {
		t.Fatal("successful browser attempt unexpectedly removed tools")
	}
}

func TestGuardBrowserRetryLoopLeavesNonBrowserRequestByteIdentical(t *testing.T) {
	body := []byte(`{"model":"hy3","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"运行 git status"}]}]}`)
	if got := guardBrowserRetryLoop(body); string(got) != string(body) {
		t.Fatalf("non-browser request changed: %s", got)
	}
}

type browserRetryTestAttempt struct {
	id     string
	output string
}

func browserRetryTestBody(attempts []browserRetryTestAttempt) []byte {
	input := []any{
		map[string]any{
			"type": "additional_tools",
			"tools": []any{map[string]any{
				"type": "custom",
				"name": "exec",
			}},
		},
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": "用 [@浏览器](plugin://browser@openai-bundled) 打开 google.com 搜索 Linux",
			}},
		},
	}
	for _, attempt := range attempts {
		input = append(input,
			map[string]any{
				"type": "custom_tool_call",
				"id": attempt.id,
				"call_id": attempt.id,
				"name": "exec",
				"input": `const r = await tools.mcp__node_repl__js({code:"const tab = await iab.tabs.create()",title:"browser"}); text(r.output);`,
			},
			map[string]any{
				"type": "custom_tool_call_output",
				"call_id": attempt.id,
				"output": attempt.output,
			},
		)
	}
	body, _ := json.Marshal(map[string]any{
		"model": "hy3",
		"tools": []any{map[string]any{
			"type": "custom",
			"name": "exec",
		}},
		"tool_choice": "auto",
		"parallel_tool_calls": true,
		"input": input,
	})
	return body
}

func countBrowserRecoveryMessages(input []any) int {
	count := 0
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		if strings.Contains(bridgeExtractContent(item["content"]), browserRecoveryMarker) {
			count++
		}
	}
	return count
}

func browserRecoveryText(input []any) string {
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		text := bridgeExtractContent(item["content"])
		if strings.Contains(text, browserRecoveryMarker) {
			return text
		}
	}
	return ""
}

func hasAdditionalToolsItem(input []any) bool {
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		if typ, _ := item["type"].(string); typ == "additional_tools" {
			return true
		}
	}
	return false
}
