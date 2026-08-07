package upstream

import (
	"encoding/json"
	"testing"
)

func TestPrepareBodyDisablesDeepSeekFlashThinkingForTools(t *testing.T) {
	in := []byte(`{
		"model":"deepseek-v4-flash",
		"reasoning_effort":"medium",
		"messages":[{"role":"user","content":"create a file"}],
		"tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object"}}}],
		"tool_choice":"auto"
	}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(in), &got); err != nil {
		t.Fatal(err)
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking == nil || thinking["type"] != "disabled" {
		t.Fatalf("thinking not disabled: %#v", got["thinking"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("reasoning effort not normalized to low: %#v", got["reasoning_effort"])
	}
}

func TestPrepareBodyLeavesPlainFlashChatThinkingUntouched(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-flash","reasoning_effort":"medium","messages":[{"role":"user","content":"hello"}]}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(in), &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["thinking"]; exists {
		t.Fatalf("plain chat unexpectedly received thinking override: %#v", got)
	}
	if got["reasoning_effort"] != "medium" {
		t.Fatalf("plain chat effort changed: %#v", got["reasoning_effort"])
	}
}

func TestPrepareBodyLeavesDeepSeekProToolsUntouched(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-pro","reasoning_effort":"high","messages":[{"role":"user","content":"work"}],"tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object"}}}]}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(in), &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["thinking"]; exists {
		t.Fatalf("pro unexpectedly received flash override: %#v", got)
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("pro effort changed: %#v", got["reasoning_effort"])
	}
}

func TestPrepareBodyToolChoiceNoneDoesNotForceFlashOverride(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-flash","reasoning_effort":"medium","messages":[{"role":"user","content":"answer only"}],"tools":[{"type":"function","function":{"name":"exec","parameters":{"type":"object"}}}],"tool_choice":"none"}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(in), &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["tools"]; exists {
		t.Fatalf("tools not removed for tool_choice none: %#v", got)
	}
	if _, exists := got["thinking"]; exists {
		t.Fatalf("non-tool request unexpectedly received thinking override: %#v", got)
	}
}
