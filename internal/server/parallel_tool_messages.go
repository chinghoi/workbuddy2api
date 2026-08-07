package server

import "encoding/json"

// mergeParallelAssistantToolCalls converts consecutive assistant messages that
// each contain exactly one tool call into the single assistant message required
// by the Chat Completions protocol for parallel tool calls. Responses API
// represents each function_call as its own input item, so a direct item-by-item
// conversion otherwise produces an invalid history:
//
//   assistant(tool_call A)
//   assistant(tool_call B)
//   tool(output A)
//   tool(output B)
//
// The valid Chat Completions shape is one assistant message containing A and B,
// followed by their tool result messages.
func mergeParallelAssistantToolCalls(body []byte) []byte {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return body
	}
	messages, ok := request["messages"].([]any)
	if !ok || len(messages) < 2 {
		return body
	}

	merged := make([]any, 0, len(messages))
	changed := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok || !isToolCallOnlyAssistantMessage(message) {
			merged = append(merged, rawMessage)
			continue
		}

		if len(merged) > 0 {
			if previous, ok := merged[len(merged)-1].(map[string]any); ok && isToolCallOnlyAssistantMessage(previous) {
				previousCalls, _ := previous["tool_calls"].([]any)
				currentCalls, _ := message["tool_calls"].([]any)
				previous["tool_calls"] = append(previousCalls, currentCalls...)
				changed = true
				continue
			}
		}
		merged = append(merged, message)
	}
	if !changed {
		return body
	}
	request["messages"] = merged
	encoded, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return encoded
}

func isToolCallOnlyAssistantMessage(message map[string]any) bool {
	role, _ := message["role"].(string)
	if role != "assistant" {
		return false
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return false
	}
	content, exists := message["content"]
	if !exists || content == nil || content == "" {
		return true
	}
	return false
}
