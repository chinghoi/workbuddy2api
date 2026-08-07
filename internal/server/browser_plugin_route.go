package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const browserPluginURI = "plugin://browser@openai-bundled"
const browserRoutingMarker = "[workbuddy2api browser routing]"

var browserSkillPathPattern = regexp.MustCompile(`browser:control-in-app-browser:[^\n]*\(file:\s*([^)]+/SKILL\.md)\)`)

// injectBrowserPluginRouting adds a targeted developer instruction when the
// user explicitly selects ChatGPT.app's bundled Browser plugin. The Browser is
// represented by a skill plus the deferred mcp__node_repl__js bridge; it is not
// a top-level function tool and must not be mistaken for a missing connector.
func injectBrowserPluginRouting(body []byte) []byte {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return body
	}
	input, ok := request["input"].([]any)
	if !ok || isTaskTitleGenerationRequest(input) {
		return body
	}

	userIndex := -1
	var context strings.Builder
	for index, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		text := bridgeExtractContent(item["content"])
		if text != "" {
			context.WriteString(text)
			context.WriteByte('\n')
		}
		role, _ := item["role"].(string)
		if role == "user" && strings.Contains(text, browserPluginURI) {
			userIndex = index
		}
	}
	if userIndex < 0 || strings.Contains(context.String(), browserRoutingMarker) {
		return body
	}

	skillPath := ""
	if match := browserSkillPathPattern.FindStringSubmatch(context.String()); len(match) == 2 {
		skillPath = strings.TrimSpace(match[1])
		if !strings.HasPrefix(skillPath, "/") || len(skillPath) > 2048 {
			skillPath = ""
		}
	}

	routingItem := map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": browserRoutingInstruction(skillPath),
		}},
	}

	input = append(input, nil)
	copy(input[userIndex+1:], input[userIndex:])
	input[userIndex] = routingItem
	request["input"] = input

	encoded, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return encoded
}

func isTaskTitleGenerationRequest(input []any) bool {
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		role, _ := item["role"].(string)
		if role != "user" {
			continue
		}
		text := strings.ToLower(bridgeExtractContent(item["content"]))
		if strings.Contains(text, "provide a short title for a task") &&
			strings.Contains(text, "user prompt") {
			return true
		}
	}
	return false
}

func browserRoutingInstruction(skillPath string) string {
	readSkill := "Read the complete browser:control-in-app-browser SKILL.md from the path listed in the available-skills catalog before taking any browser action."
	if skillPath != "" {
		readSkill = fmt.Sprintf(
			"First read the complete skill file at %s with tools.exec_command and emit its output with text(...).",
			shellQuote(skillPath),
		)
	}

	return browserRoutingMarker + " " +
		"The user explicitly selected plugin://browser@openai-bundled. Treat browser:control-in-app-browser as already available for this turn; it is a skill backed by a deferred runtime tool, not an installable connector. " +
		"Do not call request_plugin_install or plugin-management permission/dependency tools. " +
		readSkill + " " +
		"Follow that skill exactly and call tools.mcp__node_repl__js directly as the trusted in-app Browser bridge. The nested call shape is await tools.mcp__node_repl__js({code, title, timeout_ms}); only run tool discovery if that direct call reports unavailable. Do not enumerate ALL_TOOLS or inspect the bridge schema speculatively. " +
		"Bootstrap or import browser-client only inside the code passed to mcp__node_repl__js, never in the outer exec isolate. Read the selected browser documentation once as required, then reuse the existing browser and tab bindings. " +
		"Complete simple browser tasks with as few model/tool round trips as practical. After setup, combine navigation, visible-state inspection, UI interaction, submission, and verification in one nested JavaScript call when safe. " +
		"For human-like interaction, inspect the visible DOM first and use dom_cua click, type, and keypress on visible nodes. Do not guess a known selector and fill a hidden element before checking visibility. " +
		"Wrap each nested JavaScript snippet in an async IIFE and store only reusable handles on uniquely named globalThis properties. Avoid persistent top-level const or let declarations that can collide on later calls. " +
		"Use the cheapest state check that proves the next step. Do not request or emit screenshots unless the user asked for one or visual ambiguity requires it. " +
		"Do not substitute fetch, direct HTTP requests, codex_app__open_in_codex, Playwright/Puppeteer outside the in-skill tab API, or another browser for visible in-app Browser interaction. " +
		"Do not narrate internal setup, skill loading, bridge names, binding names, or DOM node IDs unless the user asks for diagnostics. Use the same language as the user's request, avoid a progress message before every tool call, and finish with a brief natural confirmation of the verified result. Complete the exact UI task the user requested; do not add a hard-coded site, query, or workflow."
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
