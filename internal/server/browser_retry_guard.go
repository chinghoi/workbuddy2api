package server

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	browserRetryLimit             = 2
	browserRecoveryMarker         = "[workbuddy2api browser recovery]"
	browserCaptchaMarker          = "[workbuddy2api browser captcha]"
	browserOmittedCallInput       = "// previous failed browser attempt omitted from upstream context"
	browserOmittedToolOutput      = "[previous browser attempt failed; details omitted from upstream context and retained in data/full_io]"
	browserRecentFailuresToRetain = 2
)

type browserAttemptAnalysis struct {
	consecutiveFailures int
	failedCallIDs       []string
	lastError           string
	lastOutput          string
}

// guardBrowserRetryLoop limits model-driven Browser retry loops without
// touching the complete request captured in data/full_io. Older failed calls
// are compacted before the upstream request is built. After a small number of
// consecutive failures, all current tool surfaces are removed so the model
// must stop retrying and report the failure to the user.
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
	// Tools stay available so the model can still ask the user (e.g. request_user_input).
	if captchaMarker != "" {
		input = append(input, browserGuardMessage(
			fmt.Sprintf(
				"The in-app Browser page is showing an anti-bot verification challenge (%s). Do NOT keep retrying the browser in this response, do not try alternate page variants or navigation workarounds. If the user's confirmation or manual action is required (e.g. solving a CAPTCHA), briefly tell the user what is blocking the task and ask them to confirm how to proceed; otherwise report the page state you observed.",
				captchaMarker,
			),
			browserCaptchaMarker,
		))
		request["input"] = input
		encoded, err := json.Marshal(request)
		if err != nil {
			return body
		}
		return encoded
	}

	// A later successful browser result resets the retry state. Keep only the
	// history compaction and remove stale recovery instructions.
	if analysis.consecutiveFailures == 0 {
		request["input"] = input
		encoded, err := json.Marshal(request)
		if err != nil {
			return body
		}
		return encoded
	}

	if analysis.consecutiveFailures >= browserRetryLimit {
		stripAllToolSurfaces(request, input)
		input = dropBrowserAdditionalToolsItems(input)
		input = append(input, browserGuardMessage(
			fmt.Sprintf(
				"The in-app Browser failed %d consecutive times. Do not call any tool again in this response. Briefly tell the user that the browser action could not be completed and include only the latest useful error: %s",
				analysis.consecutiveFailures,
				analysis.lastError,
			),
			browserRecoveryMarker,
		))
	} else {
		input = append(input, browserGuardMessage(
			fmt.Sprintf(
				"The previous in-app Browser attempt failed (%d/%d). Make at most one recovery tool call. Do not read the skill or docs again, do not enumerate tools, and do not try multiple API variants. Use one nested async IIFE, avoid persistent top-level const/let declarations, use a fresh current-branch tab, complete the whole UI task in that one call when possible, and emit the result explicitly with nodeRepl.write(...). If this recovery call fails again, tool access will be revoked and you must stop and report the error to the user in text. Latest error: %s",
				analysis.consecutiveFailures,
				browserRetryLimit,
				analysis.lastError,
			),
			browserRecoveryMarker,
		))
	}
	request["input"] = input

	encoded, err := json.Marshal(request)
	if err != nil {
		return body
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
			strings.Contains(text, browserRecoveryMarker) {
			return true
		}
	}
	return false
}

func analyzeBrowserAttempts(input []any) browserAttemptAnalysis {
	browserCalls := map[string]struct{}{}
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
				browserCalls[callID] = struct{}{}
			}
		case "function_call":
			name, _ := item["name"].(string)
			arguments, _ := item["arguments"].(string)
			callID := bridgeCallID(item)
			if callID != "" && isBrowserToolCall(name, arguments) {
				browserCalls[callID] = struct{}{}
			}
		case "custom_tool_call_output", "function_call_output":
			callID := bridgeCallID(item)
			if callID == "" {
				continue
			}
			if _, exists := browserCalls[callID]; !exists {
				continue
			}
			output := bridgeToolOutput(item["output"])
			analysis.lastOutput = output
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

func isBrowserFailureOutput(output string) bool {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
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
	if strings.HasPrefix(trimmed, `{"keys":`) || strings.Contains(trimmed, `"full":"object"`) {
		return true
	}

	// Strip the node_repl execution frame ("Script completed\nWall time ...\nOutput:\n")
	// so only the actual payload is judged.
	payloadLines := make([]string, 0, 8)
	for _, line := range strings.Split(lower, "\n") {
		ls := strings.TrimSpace(line)
		if ls == "" || isBrowserFrameLine(ls) {
			continue
		}
		payloadLines = append(payloadLines, ls)
	}
	if len(payloadLines) == 0 {
		return true
	}
	for _, line := range payloadLines {
		switch line {
		case "undefined", "null", "{}", "[]", "[object object]", "nan":
			continue
		default:
			return false
		}
	}
	return true
}

// isBrowserFrameLine reports whether a line belongs to the node_repl execution
// frame rather than the actual tool payload.
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

// removeBrowserRecoveryMessages drops stale guard instructions (both recovery
// and captcha markers) from developer/system messages so they cannot accumulate
// across turns.
func removeBrowserRecoveryMessages(input []any) []any {
	filtered := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if ok {
			role, _ := item["role"].(string)
			if role == "developer" || role == "system" {
				text := bridgeExtractContent(item["content"])
				if strings.Contains(text, browserRecoveryMarker) ||
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

func stripAllToolSurfaces(request map[string]any, input []any) {
	for _, key := range []string{
		"tools",
		"functions",
		"tool_choice",
		"function_call",
		"parallel_tool_calls",
	} {
		delete(request, key)
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
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
	}
}

func dropBrowserAdditionalToolsItems(input []any) []any {
	filtered := make([]any, 0, len(input))
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if ok {
			if typ, _ := item["type"].(string); typ == "additional_tools" {
				continue
			}
		}
		filtered = append(filtered, rawItem)
	}
	return filtered
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
