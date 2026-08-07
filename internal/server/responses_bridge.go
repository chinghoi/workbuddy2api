package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"workbuddy2api/internal/upstream"
)

// responsesBridge is the result of compiling a Responses API request into the
// chat-completions dialect accepted by WorkBuddy. Tools records enough original
// type information to restore custom tool calls on the response path.
type responsesBridge struct {
	ChatBody []byte
	Tools    upstream.ResponseToolMap
}

type bridgeToolCandidate struct {
	tool    map[string]any
	compact bool
}

func bridgeResponsesRequest(body []byte) responsesBridge {
	result := responsesBridge{ChatBody: body, Tools: upstream.ResponseToolMap{}}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return result
	}

	chatReq := map[string]any{}
	if model, ok := req["model"]; ok {
		chatReq["model"] = model
	}

	messages := make([]map[string]any, 0, 8)
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	toolCandidates := make([]bridgeToolCandidate, 0, 4)
	switch input := req["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input)
	case []any:
		for _, rawItem := range input {
			item, ok := rawItem.(map[string]any)
			if !ok {
				continue
			}
			if embedded, ok := item["tools"].([]any); ok {
				for _, rawTool := range embedded {
					if tool, ok := rawTool.(map[string]any); ok {
						toolCandidates = append(toolCandidates, bridgeToolCandidate{tool: tool, compact: true})
					}
				}
			}
			if msg := bridgeInputItem(item); msg != nil {
				messages = append(messages, msg)
			}
		}
	}
	chatReq["messages"] = messages

	// Standard top-level tools take precedence over same-named embedded tools.
	if topLevel, ok := req["tools"].([]any); ok {
		for _, rawTool := range topLevel {
			if tool, ok := rawTool.(map[string]any); ok {
				toolCandidates = append(toolCandidates, bridgeToolCandidate{tool: tool})
			}
		}
	}
	chatTools, specs := compileBridgeTools(toolCandidates)
	if len(chatTools) > 0 {
		chatReq["tools"] = chatTools
	}
	result.Tools = specs

	if choice, ok := req["tool_choice"]; ok {
		chatReq["tool_choice"] = bridgeToolChoice(choice)
	}
	if parallel, ok := req["parallel_tool_calls"]; ok {
		chatReq["parallel_tool_calls"] = parallel
	}
	if maxOutput, ok := req["max_output_tokens"]; ok {
		chatReq["max_tokens"] = maxOutput
	}
	if reasoning, ok := req["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" {
			chatReq["reasoning_effort"] = effort
		}
	}
	if effort, ok := req["reasoning_effort"]; ok {
		chatReq["reasoning_effort"] = effort
	}

	for _, key := range []string{
		"temperature", "top_p", "presence_penalty", "frequency_penalty",
		"stop", "seed", "user", "n", "logit_bias", "response_format",
		"stream_options", "metadata",
	} {
		if value, ok := req[key]; ok {
			chatReq[key] = value
		}
	}

	encoded, err := json.Marshal(chatReq)
	if err == nil {
		result.ChatBody = encoded
	}
	return result
}

func bridgeInputItem(item map[string]any) map[string]any {
	typ, _ := item["type"].(string)
	if typ == "" {
		if _, hasRole := item["role"]; hasRole {
			typ = "message"
		}
	}

	switch typ {
	case "message":
		role, _ := item["role"].(string)
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}
		content := bridgeExtractContent(item["content"])
		if content == "" {
			return nil
		}
		msg := map[string]any{"role": role, "content": content}
		if name, ok := item["name"].(string); ok && name != "" {
			msg["name"] = name
		}
		return msg

	case "function_call":
		callID := bridgeCallID(item)
		name, _ := item["name"].(string)
		arguments, _ := item["arguments"].(string)
		return assistantToolCall(callID, name, arguments)

	case "custom_tool_call":
		callID := bridgeCallID(item)
		name, _ := item["name"].(string)
		input, _ := item["input"].(string)
		arguments, _ := json.Marshal(map[string]string{"input": input})
		return assistantToolCall(callID, name, string(arguments))

	case "function_call_output", "custom_tool_call_output":
		return map[string]any{
			"role":         "tool",
			"tool_call_id": bridgeCallID(item),
			"content":      bridgeToolOutput(item["output"]),
		}
	}
	return nil
}

func assistantToolCall(callID, name, arguments string) map[string]any {
	return map[string]any{
		"role": "assistant",
		"tool_calls": []any{map[string]any{
			"id":   callID,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": arguments,
			},
		}},
	}
}

func bridgeCallID(item map[string]any) string {
	if callID, ok := item["call_id"].(string); ok && callID != "" {
		return callID
	}
	if id, ok := item["id"].(string); ok {
		return id
	}
	return ""
}

func bridgeToolOutput(output any) string {
	if text, ok := output.(string); ok {
		return text
	}
	if parts, ok := output.([]any); ok {
		texts := make([]string, 0, len(parts))
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n")
		}
	}
	if text := bridgeExtractContent(output); text != "" {
		return text
	}
	if encoded, err := json.Marshal(output); err == nil {
		return string(encoded)
	}
	return fmt.Sprint(output)
}

func compileBridgeTools(candidates []bridgeToolCandidate) ([]map[string]any, upstream.ResponseToolMap) {
	order := make([]string, 0, len(candidates))
	compiled := map[string]map[string]any{}
	specs := upstream.ResponseToolMap{}
	for _, candidate := range candidates {
		tool, spec, ok := compileBridgeTool(candidate.tool, candidate.compact)
		if !ok {
			continue
		}
		if _, exists := compiled[spec.Name]; !exists {
			order = append(order, spec.Name)
		}
		compiled[spec.Name] = tool
		specs[spec.Name] = spec
	}
	out := make([]map[string]any, 0, len(order))
	for _, name := range order {
		out = append(out, compiled[name])
	}
	return out, specs
}

func compileBridgeTool(tool map[string]any, compact bool) (map[string]any, upstream.ResponseToolSpec, bool) {
	typ, _ := tool["type"].(string)
	// Responses namespaces describe methods that are already exposed inside the
	// programmatic exec runtime. Chat Completions has no namespace tool type;
	// compiling one as an empty function only encourages invalid direct calls.
	if typ == "namespace" {
		return nil, upstream.ResponseToolSpec{}, false
	}
	name, description := "", ""
	var parameters any
	var strict any

	if function, ok := tool["function"].(map[string]any); ok {
		name, _ = function["name"].(string)
		description, _ = function["description"].(string)
		parameters = function["parameters"]
		strict = function["strict"]
	}
	if name == "" {
		name, _ = tool["name"].(string)
	}
	if description == "" {
		description, _ = tool["description"].(string)
	}
	if parameters == nil {
		parameters = tool["parameters"]
	}
	if parameters == nil {
		parameters = tool["input_schema"]
	}
	if strict == nil {
		strict = tool["strict"]
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, upstream.ResponseToolSpec{}, false
	}

	kind := upstream.ResponseToolFunction
	if typ == "custom" || (typ == "" && (name == "exec" || parameters == nil)) {
		kind = upstream.ResponseToolCustom
		parameters = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": "Raw text input for the tool.",
				},
			},
			"required":             []string{"input"},
			"additionalProperties": false,
		}
	}
	if parameters == nil {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if compact {
		description = compactToolDescription(name, description, kind)
	}

	function := map[string]any{"name": name, "parameters": parameters}
	if description != "" {
		function["description"] = description
	}
	if strict != nil {
		function["strict"] = strict
	}
	return map[string]any{"type": "function", "function": function}, upstream.ResponseToolSpec{
		Kind: kind,
		Name: name,
	}, true
}

func compactToolDescription(name, description string, kind upstream.ResponseToolKind) string {
	if name == "exec" && kind == upstream.ResponseToolCustom {
		return compactExecToolDescription(description)
	}
	description = strings.TrimSpace(description)
	if description == "" && kind == upstream.ResponseToolCustom {
		return "Invoke the " + name + " tool with raw text input."
	}
	const maxRunes = 1200
	if utf8.RuneCountInString(description) <= maxRunes {
		return description
	}
	runes := []rune(description)
	return string(runes[:maxRunes]) + "…"
}

var execToolDeclarationPattern = regexp.MustCompile(`declare const tools:\s*\{\s*([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)

// compactExecToolDescription keeps the operational contract needed by models
// to use ChatGPT.app's programmatic tool runtime, without forwarding the full
// multi-kilobyte schema that can trip WorkBuddy's content filter.
func compactExecToolDescription(description string) string {
	toolNames := extractExecToolNames(description)
	available := ""
	if len(toolNames) > 0 {
		available = " Known nested methods: " + strings.Join(toolNames, ", ") + "."
	}

	return "Run raw JavaScript orchestration source in a fresh V8 isolate. " +
		"Use await tools.<name>(...) for shell, file, browser, MCP, Git, and app actions. " +
		"Nested return shapes differ: exec_command uses r.output; MCP methods (tools.mcp__*) return CallToolResult content blocks, so for textual MCP output use `for (const p of (r?.content ?? [])) if (p?.type === \"text\") text(p.text);` and never assume r.output. If a nested tool returns a string, use text(r). " +
		"Call text(value) to emit output; bare expressions are discarded. " +
		"Do not send shell syntax directly and do not use console, require, process, Node imports, direct file-system APIs, or direct network APIs. " +
		"Input must be raw JavaScript source, not JSON or Markdown. " +
		"To discover omitted methods use text(ALL_TOOLS.map(x => x.name).join(\"\\n\"))." + available
}

func extractExecToolNames(description string) []string {
	matches := execToolDeclarationPattern.FindAllStringSubmatch(description, -1)
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		if len(match) < 2 || match[1] == "" {
			continue
		}
		seen[match[1]] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	const maxNames = 24
	if len(names) > maxNames {
		names = names[:maxNames]
	}
	return names
}

func bridgeToolChoice(choice any) any {
	obj, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	typ, _ := obj["type"].(string)
	if typ != "custom" {
		return choice
	}
	name, _ := obj["name"].(string)
	if name == "" {
		return "auto"
	}
	return map[string]any{
		"type":     "function",
		"function": map[string]any{"name": name},
	}
}

func bridgeExtractContent(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var sb strings.Builder
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := part["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				if text, ok := part["text"].(string); ok {
					sb.WriteString(text)
				}
			case "input_image", "output_image":
				if url := bridgeImageURL(part["image_url"]); url != "" {
					sb.WriteString("![image](" + url + ")")
				}
			case "input_audio", "output_audio":
				if transcript, ok := part["transcript"].(string); ok && transcript != "" {
					sb.WriteString("[audio: " + transcript + "]")
				} else if url, ok := part["url"].(string); ok && url != "" {
					sb.WriteString("[audio](" + url + ")")
				}
			case "input_file", "output_file":
				if url, ok := part["url"].(string); ok && url != "" {
					sb.WriteString("[file](" + url + ")")
				} else if fileID, ok := part["file_id"].(string); ok && fileID != "" {
					sb.WriteString("[file: " + fileID + "]")
				}
			default:
				if text, ok := part["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func bridgeImageURL(value any) string {
	switch url := value.(type) {
	case string:
		return url
	case map[string]any:
		text, _ := url["url"].(string)
		return text
	}
	return ""
}
