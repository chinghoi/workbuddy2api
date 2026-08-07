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

// injectBrowserPluginRouting adds a short, targeted developer instruction when
// ChatGPT.app's browser plugin is explicitly selected. The browser integration
// is represented by a skill plus the deferred mcp__node_repl__js bridge; it is
// not a top-level function tool and must not be mistaken for a missing plugin.
func injectBrowserPluginRouting(body []byte) []byte {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return body
	}
	input, ok := request["input"].([]any)
	if !ok {
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

	instruction := browserRoutingInstruction(skillPath)
	routingItem := map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": instruction,
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
		"Follow that skill exactly. Use tools.mcp__node_repl__js as the trusted in-app Browser control bridge described by the skill for navigation, page inspection, clicking, typing, and screenshots. " +
		"This bridge is not a normal Node.js runtime: do not use require, Node modules, Playwright, Puppeteer, or direct filesystem APIs. " +
		"Do not substitute fetch, direct HTTP requests, codex_app__open_in_codex, or another browser for visible browser interaction. " +
		"For this task, completion requires opening baidu.com in the in-app Browser, locating the visible search input, entering DeepSeek through normal UI input events, submitting the search, and verifying the resulting visible page."
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
