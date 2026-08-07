package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	browserRetryLimit             = 2
	browserRecoveryMarker         = "[workbuddy2api browser recovery]"
	browserForwardingMarker       = "[workbuddy2api browser forwarding]"
	browserCaptchaMarker          = "[workbuddy2api browser captcha]"
	browserOmittedCallInput       = "// previous failed browser attempt omitted from upstream context"
	browserOmittedToolOutput      = "[previous browser attempt failed; details omitted from upstream context and retained in data/full_io]"
	browserRecentFailuresToRetain = 2
)

type browserAttemptAnalysis struct {
	consecutiveFailures       int
	failedCallIDs             []string
	lastError                 string
	lastOutput                string
	lastCallInput             string
	lastWasForwardingFailure  bool
}

// guardBrowserRetryLoop limits model-driven Browser retry loops without
// touching the complete request captured in data/full_io. It distinguishes a
// real browser/runtime failure from an observability failure in the outer exec
// wrapper. In particular, MCP tools return CallToolResult content blocks, so
// `text(r.output)` can surface only "undefined" even though the nested browser
// call actually ran. That case must not be counted as a Browser failure or
// blindly replayed.
func guardBrowserRetryLoop(body []byte) []byte {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return body
	}
	input, ok := request["input"].([]any)
	if !ok || !containsExplicitBrowserTask(input) {
		return body
	}

	analysis := analyzeBrowserAttempts(input)
	captchaMarker := browserCaptchaChallenge(analysis.lastOutput)
	if len(analysis.failedCallIDs) == 0 && captchaMarker == "" {
		return body
	}

	input = removeBrowserRecoveryMessages(input)
	compactBrowserFailureHistory(input, analysis.failedCallIDs)

	// Anti-bot page: stop automated retries before any recovery/breaker logic.
	// Tools stay available so the model can still ask the user for manual action.
	if captchaMarker != "" {
		input = append(input, browserGuardMessage(
			fmt.Sprintf(
				"The in-app Browser page is showing an anti-bot verification challenge (%s). Do not keep retrying the browser, alternate page variants, or navigation workarounds. If manual action is required, briefly tell the user what is blocking the task and ask them to complete or approve the required step; otherwise report the verified page state.",
				captchaMarker,
			),
			browserCaptchaMarker,
		))
		request["input"] = input
		return marshalBrowserGuardRequest(body, request)
	}

	// The nested MCP call may have run successfully, but the outer exec wrapper
	// read the shell-specific `.output` field from a CallToolResult. Do not treat
	// this as a browser runtime failure and do not replay unknown side effects.
	if analysis.lastWasForwardingFailure {
		input = append(input, browserGuardMessage(
			"The previous in-app Browser nested MCP call was not surfaced correctly: the outer exec code read `.output`, but mcp__node_repl__js returns an MCP CallToolResult whose textual data is in `content` blocks. This is an output-forwarding failure, not evidence that the Browser itself failed. On the next call, never use r.output for mcp__node_repl__js; if the result is a string call text(r), otherwise iterate (r?.content ?? []) and emit only text parts with text(part.text). Do not stringify the full MCP result because _meta may contain screenshot base64. Do not blindly repeat a potentially state-changing click, submit, send, save, purchase, or delete operation just because its verification output was lost; first choose the safest idempotent/read-only verification or explain that the prior action could not be verified. For read-only navigation/search/setup, one corrected retry is acceptable.",
			browserForwardingMarker,
		))
		request["input"] = input
		return marshalBrowserGuardRequest(body, request)
	}

	// A later successful browser result resets the retry state. Keep only the
	// history compaction and remove stale guard instructions.
	if analysis.consecutiveFailures == 0 {
		request["input"] = input
		return marshalBrowserGuardRequest(body, request)
	}

	if analysis.consecutiveFailures >= browserRetryLimit {
		input = stripBrowserToolSurfaces(request, input)
		input = append(input, browserGuardMessage(
			fmt.Sprintf(
				"The in-app Browser runtime failed %d consecutive times. Do not call the Browser bridge or the exec tool again in this response. Other non-browser tools may remain available when they are genuinely useful (for example, asking the user for required input). Briefly report that the browser action could not be completed and include only the latest useful runtime error: %s",
				analysis.consecutiveFailures,
				analysis.lastError,
			),
			browserRecoveryMarker,
		))
	} else {
		input = append(input, browserGuardMessage(
			fmt.Sprintf(
				"The previous in-app Browser runtime attempt failed (%d/%d). Make at most one recovery Browser call. Do not read the skill or docs again, do not enumerate tools, and do not try multiple API variants. Use one nested async IIFE, avoid persistent top-level const/let declarations, use a fresh current-branch tab when recovery really requires a new tab, complete the whole UI task in that one call when possible, and emit the nested MCP result from its text content blocks rather than r.output. If this recovery call fails again with an explicit runtime error, Browser/exec access will be revoked for this response and you must stop and report the error. Latest runtime error: %s",
				analysis.consecutiveFailures,
				browserRetryLimit,
				analysis.lastError,
			),
			browserRecoveryMarker,
		))
	}
	request["input"] = input
	return marshalBrowserGuardRequest(body, request)
}

func marshalBrowserGuardRequest(fallback []byte, request map[string]any) []byte {
	encoded, err := json.Marshal(request)
	if err != nil {
		return fallback
	}
	return encoded
}

func containsExplicitBrowserTask(input []any) bool {
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		text := bridgeExtractContent(item["content"])
		if strings.Contains(text, browserPluginURI) ||
			strings.Contains(text, browserRoutingMarker) ||
			strings.Contains(text, browserRecoveryMarker) ||
			strings.Contains(text, browserForwardingMarker) {
			return true
		}
	}
	return false
}

func analyzeBrowserAttempts(input []any) browserAttemptAnalysis {
	browserCalls := map[string]string{}
	analysis := browserAttemptAnalysis{}

	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "custom_tool_call":
			name, _ := item["name"].(string)
			callInput, _ := item["input"].(string)
			callID := bridgeCallID(item)
			if callID != "" && isBrowserToolCall(name, callInput) {
				browserCalls[callID] = callInput
			}
		case "function_call":
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			callID := bridgeCallID(item)
			if callID != "" && isBrowserToolCall(name, arguments) {
				browserCalls[callID] = arguments
			}
		case "custom_tool_call_output", "function_call_output":
			callID := bridgeCallID(item)
			if callID == "" {
				continue
			}
			callInput, exists := browserCalls[callID]
			if !exists {
				continue
			}
			output := bridgeToolOutput(item["output"])
			analysis.lastOutput = output
			analysis.lastCallInput = callInput

			if isBrowserForwardingFailure(callInput, output) {
				analysis.consecutiveFailures = 0
				analysis.failedCallIDs = append(analysis.failedCallIDs, callID)
				analysis.lastError = "nested MCP CallToolResult was read through .output and surfaced no usable text"
				analysis.lastWasForwardingFailure = true
				continue
			}

			analysis.lastWasForwardingFailure = false
			if isBrowserFailureOutput(output) {
				analysis.consecutiveFailures++
				analysis.failedCallIDs = append(analysis.failedCallIDs, callID)
				analysis.lastError = compactBrowserError(output)
			} else {
				analysis.consecutiveFailures = 0
			}
		}
	}
	return analysis
}

func isBrowserToolCall(name, input string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name != "exec" && name != "mcp__node_repl__js" {
		return false
	}
	lower := strings.ToLower(input)
	for _, marker := range []string{
		"mcp__node_repl__js",
		"browser-client",
		"noderepl",
		"iab.",
		"control-in-app-browser",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isBrowserForwardingFailure recognizes the exact failure mode captured from
// ChatGPT.app: a nested MCP tool was invoked, but the outer exec wrapper used
// the shell-specific r.output field and therefore emitted only undefined/empty
// output. The browser call may already have executed, so this must not trigger
// blind replay or count toward the runtime circuit breaker.
func isBrowserForwardingFailure(callInput, output string) bool {
	lowerInput := strings.ToLower(callInput)
	if !strings.Contains(lowerInput, "mcp__node_repl__js") ||
		!strings.Contains(lowerInput, ".output") {
		return false
	}
	return browserOutputPayloadMissing(output)
}

func isBrowserFailureOutput(output string) bool {
	payloadLines := browserPayloadLines(output)
	if len(payloadLines) == 0 {
		return true
	}
	payload := strings.Join(payloadLines, "\n")
	lower := strings.ToLower(payload)
	for _, marker := range []string{
		"has already been declared",
		"is not a function",
		"referenceerror",
		"typeerror",
		"syntaxerror",
		"cannot read properties of undefined",
		"cannot read properties of null",
		"is not defined",
		"unexpected token",
		"unexpected identifier",
		"unexpected end of input",
		"invalid or unexpected token",
		"illegal return statement",
		"unsupported import in exec",
		"privileged native pipe bridge is not available",
		"browser-client is not trusted",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	// Serialized object instead of usable content (e.g. {"keys":...,"full":"object"})
	// is a failure in the browser automation context.
	trimmedPayload := strings.TrimSpace(payload)
	if strings.HasPrefix(trimmedPayload, `{"keys":`) || strings.Contains(trimmedPayload, `"full":"object"`) {
		return true
	}
	return browserPayloadLinesMissing(payloadLines)
}

func browserOutputPayloadMissing(output string) bool {
	return browserPayloadLinesMissing(browserPayloadLines(output))
}

func browserPayloadLinesMissing(lines []string) bool {
	if len(lines) == 0 {
		return true
	}
	for _, line := range lines {
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "undefined", "null", "{}", "[]", "[object object]", "nan":
			continue
		default:
			return false
		}
	}
	return true
}

// browserPayloadLines strips the node_repl execution frame
// ("Script completed\nWall time ...\nOutput:\n") so callers judge only the
// actual nested tool payload.
func browserPayloadLines(output string) []string {
	lines := make([]string, 0, 8)
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isBrowserFrameLine(strings.ToLower(trimmed)) {
			continue
		}
		lines = append(lines, trimmed)
	}
	return lines
}

func isBrowserFrameLine(line string) bool {
	for _, prefix := range []string{
		"script completed",
		"script failed",
		"wall time",
		"output:",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// browserCaptchaChallenge returns a challenge marker when the latest browser
// output looks like an anti-bot page, or "" when it does not.
func browserCaptchaChallenge(output string) string {
	if output == "" {
		return ""
	}
	lower := strings.ToLower(output)
	for _, marker := range []string{
		"captcha",
		"antispider",
		"anti-spider",
		"anti spider",
		"verify you are human",
		"verification required",
		"安全验证",
		"滑动验证",
		"geetest",
	} {
		if strings.Contains(lower, marker) {
			return marker
		}
	}
	return ""
}

func compactBrowserError(output string) string {
	text := strings.TrimSpace(output)
	if text == "" {
		return "empty tool output"
	}
	text = strings.Join(strings.Fields(text), " ")
	const maxRunes = 320
	runes := []rune(text)
	if len(runes) > maxRunes {
		text = string(runes[:maxRunes]) + "…"
	}
	return text
}

// removeBrowserRecoveryMessages drops stale guard instructions so they cannot
// accumulate across turns.
func removeBrowserRecoveryMessages(input []any) []any {
	filtered := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if ok {
			role, _ := item["role"].(string)
			if role == "developer" || role == "system" {
				text := bridgeExtractContent(item["content"])
				if strings.Contains(text, browserRecoveryMarker) ||
					strings.Contains(text, browserForwardingMarker) ||
					strings.Contains(text, browserCaptchaMarker) {
					continue
				}
			}
		}
		filtered = append(filtered, rawItem)
	}
	return filtered
}

func compactBrowserFailureHistory(input []any, failedCallIDs []string) {
	if len(failedCallIDs) <= browserRecentFailuresToRetain {
		return
	}
	compactIDs := map[string]struct{}{}
	for _, callID := range failedCallIDs[:len(failedCallIDs)-browserRecentFailuresToRetain] {
		compactIDs[callID] = struct{}{}
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		callID := bridgeCallID(item)
		if _, shouldCompact := compactIDs[callID]; !shouldCompact {
			continue
		}
		typ, _ := item["type"].(string)
		switch typ {
		case "custom_tool_call":
			item["input"] = browserOmittedCallInput
		case "function_call":
			arguments, _ := json.Marshal(map[string]string{"input": browserOmittedCallInput})
			item["arguments"] = string(arguments)
		case "custom_tool_call_output", "function_call_output":
			item["output"] = browserOmittedToolOutput
		}
	}
}

// stripBrowserToolSurfaces removes only the Browser-driving programmatic exec
// surface after the runtime circuit breaker opens. It intentionally preserves
// unrelated tools such as request_user_input instead of revoking every tool in
// the response.
func stripBrowserToolSurfaces(request map[string]any, input []any) []any {
	for _, key := range []string{"tools", "functions"} {
		if value, exists := request[key]; exists {
			if filtered, keep := filterBrowserRuntimeTools(value); keep {
				request[key] = filtered
			} else {
				delete(request, key)
			}
		}
	}

	filteredInput := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			filteredInput = append(filteredInput, rawItem)
			continue
		}
		for _, key := range []string{"tools", "functions"} {
			if value, exists := item[key]; exists {
				if filtered, keep := filterBrowserRuntimeTools(value); keep {
					item[key] = filtered
				} else {
					delete(item, key)
				}
			}
		}
		if typ, _ := item["type"].(string); typ == "additional_tools" {
			if _, hasTools := item["tools"]; !hasTools {
				if _, hasFunctions := item["functions"]; !hasFunctions {
					continue
				}
			}
		}
		filteredInput = append(filteredInput, item)
	}

	if browserToolChoiceTargetsRuntime(request["tool_choice"]) {
		request["tool_choice"] = "auto"
	}
	if !hasCallableToolSurface(request, filteredInput) {
		delete(request, "tool_choice")
		delete(request, "parallel_tool_calls")
	}
	return filteredInput
}

func filterBrowserRuntimeTools(value any) ([]any, bool) {
	tools, ok := value.([]any)
	if !ok {
		return nil, false
	}
	filtered := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			filtered = append(filtered, rawTool)
			continue
		}
		if isBrowserRuntimeToolName(responseToolName(tool)) {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered, len(filtered) > 0
}

func responseToolName(tool map[string]any) string {
	if name, _ := tool["name"].(string); name != "" {
		return name
	}
	if function, ok := tool["function"].(map[string]any); ok {
		name, _ := function["name"].(string)
		return name
	}
	return ""
}

func isBrowserRuntimeToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "exec", "mcp__node_repl__js":
		return true
	default:
		return false
	}
}

func browserToolChoiceTargetsRuntime(choice any) bool {
	obj, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	if isBrowserRuntimeToolName(responseToolName(obj)) {
		return true
	}
	if function, ok := obj["function"].(map[string]any); ok {
		name, _ := function["name"].(string)
		return isBrowserRuntimeToolName(name)
	}
	return false
}

func hasCallableToolSurface(request map[string]any, input []any) bool {
	for _, key := range []string{"tools", "functions"} {
		if hasCallableToolList(request[key]) {
			return true
		}
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"tools", "functions"} {
			if hasCallableToolList(item[key]) {
				return true
			}
		}
	}
	return false
}

func hasCallableToolList(value any) bool {
	tools, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := tool["type"].(string)
		if typ == "namespace" {
			continue
		}
		if responseToolName(tool) != "" {
			return true
		}
	}
	return false
}

func browserGuardMessage(text, marker string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": marker + " " + text,
		}},
	}
}
