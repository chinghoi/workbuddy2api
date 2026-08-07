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
		"For mcp__node_repl__js, the outer exec result is an MCP CallToolResult, not an exec_command result. Do not read r.output. Forward only textual MCP content: if the nested result is a string, call text(r); otherwise iterate (r?.content ?? []) and call text(part.text) only for parts whose type is text. Never stringify the full MCP result because _meta can contain screenshot base64. " +
		"Bootstrap or import browser-client only inside the code passed to mcp__node_repl__js, never in the outer exec isolate. Read the selected browser documentation once as required, then reuse the existing browser binding. " +
		"An edited root message or a newly selected conversation branch is a fresh task branch. Reuse only the browser binding; do not recover, assume, or reuse a tab created by an earlier branch. Unless the user explicitly mentions an existing tab, create a fresh tab for the current request. Prefer completing navigation, visible-state inspection, interaction, submission, and verification in one nested JavaScript call so no tab recovery is needed. If the current request must span multiple nested calls, store only the tab created or claimed for this current request on a uniquely named globalThis property and use that exact handle directly. Do not call browser.tabs.list(), browser.user.openTabs(), or an equivalent tab-listing method merely to recover that current-request tab. " +
		"Nested JavaScript return values are not automatically surfaced. Do not rely on return { ... } as the tool result. Inside the nested code, emit the small textual verification result explicitly with nodeRepl.write(...), for example nodeRepl.write(JSON.stringify({url: info.url, title: info.title})). The outer exec call must then forward the nested CallToolResult text blocks using the rule above. " +
		"Complete simple browser tasks with as few model/tool round trips as practical. After setup, combine navigation, visible-state inspection, UI interaction, submission, and verification in one nested JavaScript call when safe. " +
		"For human-like interaction, inspect the visible DOM first and use dom_cua click, type, and keypress on visible nodes. Do not guess a known selector and fill a hidden element before checking visibility. " +
		"Wrap each nested JavaScript snippet in an async IIFE and store only reusable current-request handles on uniquely named globalThis properties. Avoid persistent top-level const or let declarations that can collide on later calls. " +
		"The nested browser tool automatically attaches screenshot data in _meta even when no screenshot was requested. Never emit the complete nested result with text(JSON.stringify(r)) or an equivalent full dump. Emit only content entries whose type is text, or a small hand-built object derived from those text entries. This is required to keep screenshot base64 out of the desktop conversation and prevent unnecessary context checkpoint compaction. " +
		"Use the cheapest state check that proves the next step. Do not request or emit screenshots unless the user asked for one or visual ambiguity requires it. Verify success from textual URL, title, and visible-DOM results returned by the bridge, and only report page details actually present in that textual evidence. " +
		"Do not substitute fetch, direct HTTP requests, codex_app__open_in_codex, Playwright/Puppeteer outside the in-skill tab API, or another browser for visible in-app Browser interaction. " +
		"Do not narrate internal setup, skill loading, bridge names, binding names, or DOM node IDs unless the user asks for diagnostics. Use the same language as the user's request, avoid a progress message before every tool call, and finish with a brief natural confirmation of the verified result. Complete the exact UI task the user requested; do not add a hard-coded site, query, or workflow."
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
