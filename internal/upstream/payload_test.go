package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSanitizeBlockedTemplates(t *testing.T) {
	// 真实 Claude Code 模板（取自 2.1.201 binary，PR #3 修正）
	in := "You are Claude Code, Anthropic's official CLI tool for Claude.\nDefault branch (you will usually use this for PRs)"
	out := sanitizeBlockedTemplates(in)
	if !strings.Contains(out, "command-line tool for Claude.") {
		t.Errorf("identity sentence not rewritten: %q", out)
	}
	if strings.Contains(out, "CLI tool for Claude.") {
		t.Errorf("real identity sentence survived: %q", out)
	}
	if !strings.Contains(out, "Primary branch (you will usually use this for PRs)") {
		t.Errorf("branch sentence not rewritten: %q", out)
	}
	if strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("real branch sentence survived: %q", out)
	}
	// 兼容旧假设形态（防御性，应同样收敛到安全形态）
	legacy := sanitizeBlockedTemplates("You are Claude Code, Anthropic's official CLI for Claude.\nMain branch (you will usually use this for PRs)")
	if !strings.Contains(legacy, "command-line tool for Claude.") || !strings.Contains(legacy, "Primary branch") {
		t.Errorf("legacy template not fully rewritten: %q", legacy)
	}
	// 无模板句时原样返回
	if got := sanitizeBlockedTemplates("hello world"); got != "hello world" {
		t.Errorf("unexpected rewrite: %q", got)
	}
}

func TestSanitizeBlockedTemplatesIn(t *testing.T) {
	// system 字符串（真实模板）
	obj := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "Default branch (you will usually use this for PRs)"},
			map[string]any{"role": "user", "content": "Default branch (you will usually use this for PRs)"},
		},
	}
	sanitizeBlockedTemplatesIn(obj)
	msgs := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "Primary branch (you will usually use this for PRs)") {
		t.Errorf("system not sanitized: %v", sys)
	}
	if strings.Contains(sys, "Default branch (you will usually use this for PRs)") {
		t.Errorf("real branch sentence survived in system: %v", sys)
	}
	// 非 system 消息不受影响
	if usr, _ := msgs[1].(map[string]any); strings.Contains(usr["content"].(string), "Primary branch") {
		t.Errorf("user message wrongly sanitized: %v", usr["content"])
	}

	// system 为 parts 数组（真实模板）
	obj2 := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": []any{
				map[string]any{"type": "text", "text": "You are Claude Code, Anthropic's official CLI tool for Claude."},
			}},
		},
	}
	sanitizeBlockedTemplatesIn(obj2)
	arr := obj2["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if !strings.Contains(arr[0].(map[string]any)["text"].(string), "command-line tool for Claude.") {
		t.Errorf("system array not sanitized: %v", arr[0])
	}

	// 顶层 instructions 也需中和
	obj3 := map[string]any{"instructions": "Default branch (you will usually use this for PRs)"}
	sanitizeBlockedTemplatesIn(obj3)
	if !strings.Contains(obj3["instructions"].(string), "Primary branch (you will usually use this for PRs)") {
		t.Errorf("instructions not sanitized: %v", obj3["instructions"])
	}
}

func TestForceMaxThinking(t *testing.T) {
	cases := []struct {
		model      string
		preEffort  string // "" = 客户端未传
		wantEffort string // "" = 调用后不应存在；"high" = 调用后应为 high
	}{
		{"hy3", "", "high"},
		{"hy3-preview", "medium", "high"},
		{"hy3-preview-agent", "xhigh", "high"},
		{"hy3", "high", "high"}, // 已 high，保持不变
		{"deepseek-v4-flash", "", ""}, // 非 hy3 不应被增设
		{"glm-5.2", "high", "high"}, // 非 hy3：函数 no-op，保留客户端传入值
	}
	for _, c := range cases {
		obj := map[string]any{"model": c.model}
		if c.preEffort != "" {
			obj["reasoning_effort"] = c.preEffort
		}
		forceMaxThinking(obj)
		got, has := obj["reasoning_effort"]
		if c.wantEffort == "" {
			if has {
				t.Errorf("%s: unexpected reasoning_effort=%v", c.model, got)
			}
			continue
		}
		if !has || got != c.wantEffort {
			t.Errorf("%s preEffort=%q: effort=%v want %q", c.model, c.preEffort, got, c.wantEffort)
		}
	}
}

func TestPrepareBodyAppliesProtocolRewrites(t *testing.T) {
	in := []byte(`{
		"model":"hy3",
		"messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI tool for Claude.\nDefault branch (you will usually use this for PRs)"},{"role":"user","content":"hi"}]
	}`)
	out := PrepareBody(in)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if obj["stream"] != true {
		t.Errorf("stream not forced: %v", obj["stream"])
	}
	if obj["reasoning_effort"] != "high" {
		t.Errorf("hy3 reasoning_effort not forced: %v", obj["reasoning_effort"])
	}
	msgs := obj["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "CLI tool for Claude.") || strings.Contains(sys, "Default branch (you will usually use this for PRs)") {
		t.Errorf("blocked template not neutralized: %q", sys)
	}
	if !strings.Contains(sys, "command-line tool for Claude.") || !strings.Contains(sys, "Primary branch (you will usually use this for PRs)") {
		t.Errorf("blocked template not rewritten to safe form: %q", sys)
	}
}

func TestPrepareBodyNonHy3Untouched(t *testing.T) {
	in := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	out := PrepareBody(in)
	var obj map[string]any
	json.Unmarshal(out, &obj)
	if _, has := obj["reasoning_effort"]; has {
		t.Errorf("non-hy3 should not get reasoning_effort: %v", obj)
	}
	if obj["stream"] != true {
		t.Errorf("stream should be forced even for non-hy3")
	}
}
