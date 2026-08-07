package server

import "encoding/json"

// stripTaskTitleTools prevents ChatGPT.app's background task-title request from
// entering an agent/tool loop. A title request is auxiliary UI work: it should
// produce a short text label only, never execute shell, file, browser, MCP, or
// other tools.
//
// The desktop request can carry tools both at the top level and in a dedicated
// additional_tools input item. It can also retry with tool-call history after a
// model incorrectly selected a tool. Remove all of those protocol fields/items
// while preserving the actual title prompt and ordinary message context.
func stripTaskTitleTools(body []byte) []byte {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return body
	}
	input, ok := request["input"].([]any)
	if !ok || !isTaskTitleGenerationRequest(input) {
		return body
	}

	for _, key := range []string{
		"tools",
		"functions",
		"tool_choice",
		"function_call",
		"parallel_tool_calls",
	} {
		delete(request, key)
	}

	filtered := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			filtered = append(filtered, rawItem)
			continue
		}

		switch itemType, _ := item["type"].(string); itemType {
		case "additional_tools", "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
			continue
		}

		for _, key := range []string{
			"tools",
			"functions",
			"tool_choice",
			"function_call",
			"parallel_tool_calls",
		} {
			delete(item, key)
		}
		filtered = append(filtered, item)
	}
	request["input"] = filtered

	encoded, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return encoded
}
