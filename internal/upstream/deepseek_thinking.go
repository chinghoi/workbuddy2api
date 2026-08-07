package upstream

import "strings"

// normalizeDeepSeekFlashThinking keeps agent/tool requests responsive and
// compatible with the Responses bridge. DeepSeek V4 Flash enables thinking by
// default, and low/medium reasoning_effort values are compatibility-mapped to
// high while thinking mode is active. In thinking-mode tool conversations every
// subsequent turn must also replay the complete reasoning_content, which the
// Responses bridge intentionally does not expose as conversation content.
//
// For tool-bearing Flash requests, explicitly select non-thinking mode and
// remove reasoning_effort entirely. Sending effort=low is not a low-cost
// fallback for DeepSeek V4: providers that recognize the effort field can map
// it to high and effectively undo the thinking disable request. Plain chat
// requests are left untouched.
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
	delete(obj, "reasoning_effort")
}
