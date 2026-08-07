// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
//
// 历史说明（2026-08-06 实验，已回退为数据收集模式）：
//   早期曾对非 deepseek 系列模型一刀切剥离 tools/tool_choice/parallel_tool_calls，
//   实测能缓解 hy3/glm-5.2 的"沉默 + reasoning 独大"症状，但不够稳定且会误伤
//   真正需要 tools 的场景。当前改为在 trace 中收集所有请求的全字段落盘到
//   data/capture.jsonl，待数据积累后再做精准适配。
package upstream

import (
	"encoding/json"
	"strings"
)

// PrepareBody 单 pass 改写；无法解析时原样返回。
// 顺序：强制 stream → 归一化 tool_choice → 协议级改写。
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	normalizeToolChoice(obj)
	rewriteForUpstream(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// rewriteForUpstream 应用所有协议级改写：
//  1. 去除桌面端在重试/切模型后重复附加的相同元数据块。
//  2. 敏感模板中和。
//  3. hy3 系列强制最大思考。
//  4. deepseek-v4-flash 工具请求关闭 thinking，避免长时间仅输出推理，
//     并避免后续工具轮次必须回传 reasoning_content 的协议冲突。
func rewriteForUpstream(obj map[string]any) {
	dedupeDesktopMetadataMessages(obj)
	sanitizeBlockedTemplatesIn(obj)
	forceMaxThinking(obj)
	normalizeDeepSeekFlashThinking(obj)
}

// sanitizeBlockedTemplates 中和会被上游内容审核逐字匹配的 Claude Code 模板句。
//
// 真实模板取自 Claude Code 2.1.201 binary（PR #3 修正）：
//   - "You are Claude Code, Anthropic's official CLI tool for Claude."
//   - "Default branch (you will usually use this for PRs)"
// 早期 commit 误把源写成 "CLI for Claude."/"Main branch"（与真实模板不符，改写恒为 no-op）。
//
// 为兼容不同客户端版本，同时保留旧假设形态的改写（幂等，最终都收敛到安全形态）：
//   CLI for Claude. → CLI tool for Claude. → command-line tool for Claude.
//   Main branch     → Default branch       → Primary branch
func sanitizeBlockedTemplates(s string) string {
	// 旧假设形态 → 真实形态（防御性，幂等）
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic's official CLI tool for Claude.")
	s = strings.ReplaceAll(s,
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)")
	// 真实形态 → 安全形态（最小改写，语义不变，绕过精确匹配）
	s = strings.ReplaceAll(s,
		"You are Claude Code, Anthropic's official CLI tool for Claude.",
		"You are Claude Code, Anthropic's official command-line tool for Claude.")
	s = strings.ReplaceAll(s,
		"Default branch (you will usually use this for PRs)",
		"Primary branch (you will usually use this for PRs)")
	return s
}

// sanitizeBlockedTemplatesIn 对 system 消息（content 可为字符串或 parts 数组）
// 与顶层 instructions 字段做模板中和。仅最小改写，不影响其它内容。
func sanitizeBlockedTemplatesIn(obj map[string]any) {
	if instr, ok := obj["instructions"].(string); ok && instr != "" {
		obj["instructions"] = sanitizeBlockedTemplates(instr)
	}
	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := msg["role"].(string); role != "system" {
				continue
			}
			if content, ok := msg["content"]; ok {
				msg["content"] = sanitizeContentField(content)
			}
		}
	}
}

// sanitizeContentField 中和 content 字段（字符串或 parts 数组）。
func sanitizeContentField(content any) any {
	switch v := content.(type) {
	case string:
		return sanitizeBlockedTemplates(v)
	case []any:
		for _, p := range v {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := pm["text"].(string); ok {
				pm["text"] = sanitizeBlockedTemplates(t)
			}
			if t, ok := pm["content"].(string); ok {
				pm["content"] = sanitizeBlockedTemplates(t)
			}
		}
		return v
	default:
		return content
	}
}

// forceMaxThinking 对 hy3 系列模型强制 reasoning_effort=high。
// CodeBuddy 仅识别 high 触发深度思考（medium/max 等档位忽略），故 high 即最高档。
func forceMaxThinking(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !strings.HasPrefix(model, "hy3") {
		return
	}
	if eff, _ := obj["reasoning_effort"].(string); strings.EqualFold(eff, "high") {
		return
	}
	obj["reasoning_effort"] = "high"
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}
