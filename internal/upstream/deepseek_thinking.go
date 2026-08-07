package upstream

import "strings"

// normalizeDeepSeekFlashThinking keeps agent/tool requests responsive and
// compatible with the Responses bridge. DeepSeek V4 Flash enables thinking by
// default, and the desktop client's default "medium" effort is interpreted as
// high by DeepSeek. In thinking-mode tool conversations every subsequent turn
// must also replay the complete reasoning_content, which Responses clients do
// not carry on function_call items.
//
// For tool-bearing Flash requests, use non-thinking mode. Keep effort=low as a
// compatibility fallback for WorkBuddy layers that recognize reasoning_effort
// but do not forward the newer thinking object. Plain chat requests are left
// untouched.
func normalizeDeepSeekFlashThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.EqualFold(strings.TrimSpace(model), "deepseek-v4-flash") {
		return
	}
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		return
	}
	obj["thinking"] = map[string]any{"type": "disabled"}
	obj["reasoning_effort"] = "low"
}
