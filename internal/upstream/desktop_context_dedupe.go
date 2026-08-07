package upstream

import "strings"

// dedupeDesktopMetadataMessages removes exact duplicate copies of the large
// desktop metadata blocks that ChatGPT.app can append again after retries or
// model switches. It deliberately touches only the two known generated blocks;
// ordinary user/system messages are never deduplicated, even when identical.
func dedupeDesktopMetadataMessages(obj map[string]any) {
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) < 2 {
		return
	}

	seen := make(map[string]struct{}, 2)
	out := make([]any, 0, len(messages))
	changed := false
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		if !isGeneratedDesktopMetadata(role, content) {
			out = append(out, raw)
			continue
		}
		key := role + "\x00" + content
		if _, exists := seen[key]; exists {
			changed = true
			continue
		}
		seen[key] = struct{}{}
		out = append(out, raw)
	}
	if changed {
		obj["messages"] = out
	}
}

func isGeneratedDesktopMetadata(role, content string) bool {
	trimmed := strings.TrimSpace(content)
	return (role == "system" && strings.HasPrefix(trimmed, "<app-context>")) ||
		(role == "user" && strings.HasPrefix(trimmed, "<recommended_plugins>"))
}
