package upstream

import (
	"encoding/json"
	"testing"
)

func TestPrepareBodyDedupesGeneratedDesktopMetadata(t *testing.T) {
	in := []byte(`{
		"model":"deepseek-v4-flash",
		"messages":[
			{"role":"system","content":"<app-context>same desktop context"},
			{"role":"user","content":"<recommended_plugins>same plugins"},
			{"role":"system","content":"<app-context>same desktop context"},
			{"role":"user","content":"<recommended_plugins>same plugins"},
			{"role":"user","content":"actual request"},
			{"role":"user","content":"actual request"}
		]
	}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(in), &got); err != nil {
		t.Fatal(err)
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("message count=%d want 4: %#v", len(messages), messages)
	}
	if messages[0].(map[string]any)["content"] != "<app-context>same desktop context" {
		t.Fatalf("app context missing: %#v", messages)
	}
	if messages[1].(map[string]any)["content"] != "<recommended_plugins>same plugins" {
		t.Fatalf("plugin metadata missing: %#v", messages)
	}
	// Ordinary duplicate user messages are intentional conversation content and
	// must remain untouched.
	if messages[2].(map[string]any)["content"] != "actual request" ||
		messages[3].(map[string]any)["content"] != "actual request" {
		t.Fatalf("ordinary user messages changed: %#v", messages)
	}
}

func TestPrepareBodyKeepsDistinctDesktopMetadata(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"system","content":"<app-context>version one"},
			{"role":"system","content":"<app-context>version two"},
			{"role":"user","content":"hello"}
		]
	}`)
	var got map[string]any
	if err := json.Unmarshal(PrepareBody(in), &got); err != nil {
		t.Fatal(err)
	}
	messages, _ := got["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("distinct metadata was removed: %#v", messages)
	}
}
