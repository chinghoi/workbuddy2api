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
		"Make at most one recovery Browser call",
		"one nested async IIFE",
		"rather than r.output",
		"Browser/exec access will be revoked",
		"Identifier 'tab' has already been declared",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("recovery instruction missing %q: %s", expected, text)
		}
	}
	if !hasAdditionalToolsItem(input) {
		t.Fatal("single runtime failure unexpectedly removed browser tools")
	}
	if !bridgeResponsesRequest(guarded).Tools.IsCustom("exec") {
		t.Fatal("single runtime failure unexpectedly removed exec")
	}
}

func TestGuardBrowserRetryLoopStopsAfterThreeRuntimeFailuresAndCompactsHistory(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", output: "ReferenceError: tab is not defined"},
		{id: "call_2", output: "SyntaxError: Identifier 'info' has already been declared"},
		{id: "call_3", output: "TypeError: allInnerTexts is not a function"},
	})

	guarded := guardBrowserRetryLoop(body)
	var request map[string]any
	if err := json.Unmarshal(guarded, &request); err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	if !hasAdditionalToolsItem(input) {
		t.Fatal("circuit breaker should preserve non-browser additional tools")
	}
	text := browserRecoveryText(input)
	if !strings.Contains(text, "failed 3 consecutive times") ||
		!strings.Contains(text, "Do not call the Browser bridge or the exec tool again") {
		t.Fatalf("missing circuit-breaker instruction: %s", text)
	}
	bridge := bridgeResponsesRequest(guarded)
	if bridge.Tools.IsCustom("exec") {
		t.Fatal("circuit breaker still exposed exec")
	}
	if _, ok := bridge.Tools["request_user_input"]; !ok {
		t.Fatalf("circuit breaker removed unrelated request_user_input: %#v", bridge.Tools)
	}
	if _, ok := bridge.Tools["wait"]; !ok {
		t.Fatalf("circuit breaker removed unrelated wait tool: %#v", bridge.Tools)
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

func TestGuardBrowserRetryLoopMCPOutputPropertyIsForwardingFailure(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", output: "Script completed\nWall time 0.0 seconds\nOutput:\nundefined"},
	})

	guarded := guardBrowserRetryLoop(body)
	var request map[string]any
	if err := json.Unmarshal(guarded, &request); err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	if got := countBrowserRecoveryMessages(input); got != 0 {
		t.Fatalf("forwarding failure incorrectly counted as runtime failure: %d recovery messages", got)
	}
	text := browserGuardTextWithMarker(input, browserForwardingMarker)
	for _, expected := range []string{
		"output-forwarding failure, not evidence that the Browser itself failed",
		"never use r.output for mcp__node_repl__js",
		"r?.content",
		"Do not blindly repeat a potentially state-changing",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("forwarding instruction missing %q: %s", expected, text)
		}
	}
	if !bridgeResponsesRequest(guarded).Tools.IsCustom("exec") {
		t.Fatal("forwarding failure should keep exec available for corrected verification/retry")
	}
}

func TestGuardBrowserRetryLoopRepeatedForwardingFailuresDoNotOpenBreaker(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", output: "undefined"},
		{id: "call_2", output: "Script completed\nWall time 0.0 seconds\nOutput:\nundefined"},
	})
	guarded := guardBrowserRetryLoop(body)
	bridge := bridgeResponsesRequest(guarded)
	if !bridge.Tools.IsCustom("exec") {
		t.Fatal("repeated MCP forwarding mistakes must not revoke Browser/exec access")
	}
	var request map[string]any
	if err := json.Unmarshal(guarded, &request); err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	if text := browserGuardTextWithMarker(input, browserForwardingMarker); text == "" {
		t.Fatal("missing forwarding correction after repeated forwarding mistakes")
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
	input = append(input,
		browserGuardMessage("stale recovery", browserRecoveryMarker),
		browserGuardMessage("stale forwarding", browserForwardingMarker),
	)
	request["input"] = input
	body, _ = json.Marshal(request)

	guarded := guardBrowserRetryLoop(body)
	var got map[string]any
	if err := json.Unmarshal(guarded, &got); err != nil {
		t.Fatal(err)
	}
	gotInput, _ := got["input"].([]any)
	for _, marker := range []string{browserRecoveryMarker, browserForwardingMarker} {
		if text := browserGuardTextWithMarker(gotInput, marker); text != "" {
			t.Fatalf("successful browser attempt left stale %s message: %s", marker, text)
		}
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
	arr    []any // optional structured output (mirrors real tool result shape)
	input  string
}

func browserRetryTestBody(attempts []browserRetryTestAttempt) []byte {
	input := []any{
		map[string]any{
			"type": "additional_tools",
			"tools": []any{
				map[string]any{"type": "custom", "name": "exec"},
				map[string]any{
					"type": "function",
					"name": "wait",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{"cell_id": map[string]any{"type": "string"}},
						"required": []string{"cell_id"},
					},
				},
				map[string]any{
					"type": "function",
					"name": "request_user_input",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{},
					},
				},
			},
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
		output := any(attempt.output)
		if attempt.arr != nil {
			output = attempt.arr
		}
		callInput := attempt.input
		if callInput == "" {
			callInput = `const r = await tools.mcp__node_repl__js({code:"const tab = await iab.tabs.create()",title:"browser"}); text(r.output);`
		}
		input = append(input,
			map[string]any{
				"type":    "custom_tool_call",
				"id":      attempt.id,
				"call_id": attempt.id,
				"name":    "exec",
				"input":   callInput,
			},
			map[string]any{
				"type":    "custom_tool_call_output",
				"call_id": attempt.id,
				"output":  output,
			},
		)
	}
	body, _ := json.Marshal(map[string]any{
		"model":               "hy3",
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"input":               input,
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
	return browserGuardTextWithMarker(input, browserRecoveryMarker)
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

func TestIsBrowserFailureOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{"empty", "", true},
		{"bare undefined", "undefined", true},
		{"bare error", "TypeError: allInnerTexts is not a function", true},
		{"declared", "SyntaxError: Identifier 'tab' has already been declared", true},
		{"v8 typeerror", "Cannot read properties of undefined (reading 'evaluate')", true},
		{"v8 undefined var", "ReferenceError: gUrl is not defined", true},
		{"illegal return", "SyntaxError: Illegal return statement", true},
		{"serialized object", `{"keys":["content","isError"],"full":"object"}`, true},
		{"frame plus undefined", "Script completed\nWall time 0.1 seconds\nOutput:\nundefined", true},
		{"frame plus null", "Script completed\nWall time 0.5 seconds\nOutput:\nnull", true},
		{"frame plus error", "Script completed\nWall time 0.2 seconds\nOutput:\nTypeError: tab is undefined", true},
		{"frame plus empty", "Script completed\nWall time 0.2 seconds\nOutput:\n", true},
		{"page content", `{"url":"https://www.sogou.com/","title":"搜狗搜索引擎"}`, false},
		{"real payload", "搜索完成，结果如下", false},
	}
	for _, tc := range cases {
		if got := isBrowserFailureOutput(tc.output); got != tc.want {
			t.Errorf("%s: isBrowserFailureOutput(%q) = %v, want %v", tc.name, tc.output, got, tc.want)
		}
	}
}

func TestIsBrowserForwardingFailure(t *testing.T) {
	badWrapper := `const r = await tools.mcp__node_repl__js({code:"nodeRepl.write('ok')"}); text(r.output);`
	goodWrapper := `const r = await tools.mcp__node_repl__js({code:"nodeRepl.write('ok')"}); for (const p of (r?.content ?? [])) if (p?.type === "text") text(p.text);`
	if !isBrowserForwardingFailure(badWrapper, "Script completed\nOutput:\nundefined") {
		t.Fatal("expected r.output + undefined to be classified as forwarding failure")
	}
	if isBrowserForwardingFailure(goodWrapper, "Script completed\nOutput:\nundefined") {
		t.Fatal("correct MCP content extraction must not be classified as forwarding failure")
	}
	if isBrowserForwardingFailure(badWrapper, "TypeError: browser disconnected") {
		t.Fatal("explicit runtime error must not be hidden as forwarding failure")
	}
}

func TestGuardBrowserRetryLoopStopsAfterTwoRuntimeFailures(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", output: "ReferenceError: iab is not defined"},
		{id: "call_2", output: "SyntaxError: Identifier 'tab' has already been declared"},
	})

	guarded := guardBrowserRetryLoop(body)
	bridge := bridgeResponsesRequest(guarded)
	if bridge.Tools.IsCustom("exec") {
		t.Fatal("two explicit runtime failures should strip exec")
	}
	if _, ok := bridge.Tools["request_user_input"]; !ok {
		t.Fatalf("two explicit runtime failures should preserve request_user_input: %#v", bridge.Tools)
	}
	var request map[string]any
	if err := json.Unmarshal(guarded, &request); err != nil {
		t.Fatal(err)
	}
	input, _ := request["input"].([]any)
	if text := browserRecoveryText(input); !strings.Contains(text, "failed 2 consecutive times") {
		t.Fatalf("missing circuit-breaker instruction: %s", text)
	}
}

func TestGuardBrowserRetryLoopArrayOutputFailure(t *testing.T) {
	body := browserRetryTestBody([]browserRetryTestAttempt{
		{id: "call_1", arr: []any{
			map[string]any{"type": "input_text", "text": "Script completed\nWall time 0.1 seconds\nOutput:\n"},
			map[string]any{"type": "input_text", "text": "ReferenceError: iab is not defined"},
		}},
		{id: "call_2", arr: []any{
			map[string]any{"type": "input_text", "text": "Script completed\nWall time 0.1 seconds\nOutput:\n"},
			map[string]any{"type": "input_text", "text": "Cannot read properties of undefined (reading 'evaluate')"},
		}},
	})

	guarded := guardBrowserRetryLoop(body)
	if bridgeResponsesRequest(guarded).Tools.IsCustom("exec") {
		t.Fatal("array-shaped explicit runtime failures should strip exec")
	}
}

func TestGuardBrowserRetryLoopCaptchaInjectsStopInstruction(t *testing.T) {
	for _, output := range []string{
		`{"title":"安全验证","url":"https://www.sogou.com/antispider/"}`,
		`{"title":"Please verify you are human","url":"https://example.com/challenge"}`,
	} {
		body := browserRetryTestBody([]browserRetryTestAttempt{{id: "call_1", output: output}})

		guarded := guardBrowserRetryLoop(body)
		var request map[string]any
		if err := json.Unmarshal(guarded, &request); err != nil {
			t.Fatal(err)
		}
		input, _ := request["input"].([]any)
		text := browserGuardTextWithMarker(input, browserCaptchaMarker)
		if !strings.Contains(text, "anti-bot verification challenge") {
			t.Fatalf("missing captcha instruction for %q: %s", output, text)
		}
		if !hasAdditionalToolsItem(input) {
			t.Fatalf("captcha page must not strip tools for %q", output)
		}
	}
}

func browserGuardTextWithMarker(input []any, marker string) string {
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil {
			continue
		}
		text := bridgeExtractContent(item["content"])
		if strings.Contains(text, marker) {
			return text
		}
	}
	return ""
}
