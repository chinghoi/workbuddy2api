// responses.go — POST /v1/responses 端点：将 Codex 的 Responses API 请求
// 翻译为 chat completions 上游调用，再把 SSE 流转回 responses 事件。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// responses 请求体 → chat completions 请求体
	chatBody := responsesToChatBody(body)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			_ = acct.SaveAtomic()
		}

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, chatBody, r.Context())
		if terr != nil {
			lastErr = terr
			h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			kind := upstream.Classify(status, string(respBody))
			switch kind {
			case upstream.ErrHardCredit:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额不足")
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "12153 session dead")
			case upstream.ErrNotFound:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
			default:
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			continue
		}
		defer rc.Close()
		h.cfg.Pool.NoteSuccess(acct.UID)
		if peek.Stream {
			_ = upstream.StreamResponses(w, rc, peek.Model)
			return
		}
		resp, err := upstream.AggregateResponse(rc, peek.Model)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// responsesToChatBody 将 Responses API 请求体转换为 chat completions 请求体。
// 模型名原样透传（不做映射）。
//
// 注意：上游仅支持流式 chat 请求（实测 code=11101 "Non-stream chat request
// is currently not supported"），因此这里**不**设置 stream 字段——真正的
// stream:true 由 upstream.PrepareBody 统一强制（chat/completions 与
// responses 两条路径共用）。非流式调用方由本 handler 在返回侧聚合。
func responsesToChatBody(body []byte) []byte {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body
	}

	chatReq := map[string]any{}

	// model 透传
	if m, ok := req["model"]; ok {
		chatReq["model"] = m
	}

	// instructions → system message
	messages := []map[string]any{}
	if instr, ok := req["instructions"].(string); ok && instr != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instr})
	}

	// input → messages
	switch v := req["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if msg := convertInputItem(m); msg != nil {
					messages = append(messages, msg)
				}
			}
		}
	}
	chatReq["messages"] = messages

	// tools: responses 格式 {"type":"function","name":"x","parameters":{}}
	//      → chat 格式 {"type":"function","function":{"name":"x","parameters":{}}}
	if tools, ok := req["tools"].([]any); ok {
		chatTools := []map[string]any{}
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if tt, _ := tm["type"].(string); tt == "function" || tt == "" {
				fn := map[string]any{}
				if n, ok := tm["name"].(string); ok {
					fn["name"] = n
				}
				if d, ok := tm["description"].(string); ok {
					fn["description"] = d
				}
				if p, ok := tm["parameters"]; ok {
					fn["parameters"] = p
				}
				if strict, ok := tm["strict"]; ok {
					fn["strict"] = strict
				}
				chatTools = append(chatTools, map[string]any{"type": "function", "function": fn})
			} else {
				chatTools = append(chatTools, tm)
			}
		}
		if len(chatTools) > 0 {
			chatReq["tools"] = chatTools
		}
	}

	// tool_choice 透传（PrepareBody 会归一化）
	if tc, ok := req["tool_choice"]; ok {
		chatReq["tool_choice"] = tc
	}

	// max_output_tokens → max_tokens
	if mot, ok := req["max_output_tokens"]; ok {
		chatReq["max_tokens"] = mot
	}

	// parallel_tool_calls 透传
	if ptc, ok := req["parallel_tool_calls"]; ok {
		chatReq["parallel_tool_calls"] = ptc
	}

	// 标量字段透传（max_output_tokens 已在上面映射为 max_tokens）
	for _, key := range []string{
		"temperature", "top_p", "presence_penalty", "frequency_penalty",
		"stop", "seed", "user", "n", "logit_bias", "response_format",
		"stream_options", "metadata",
	} {
		if v, ok := req[key]; ok {
			chatReq[key] = v
		}
	}

	out, err := json.Marshal(chatReq)
	if err != nil {
		return body
	}
	return out
}

// convertInputItem 将 responses API input item 转换为 chat message。
func convertInputItem(item map[string]any) map[string]any {
	switch typ, _ := item["type"].(string); typ {
	case "message":
		role, _ := item["role"].(string)
		if role == "" {
			role = "user"
		}
		content := extractContent(item["content"])
		msg := map[string]any{"role": role}
		if content != "" {
			msg["content"] = content
		}
		if n, ok := item["name"].(string); ok {
			msg["name"] = n
		}
		return msg
	case "function_call":
		callID, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		return map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": callID, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}
	case "function_call_output":
		callID, _ := item["call_id"].(string)
		output, _ := item["output"].(string)
		return map[string]any{
			"role": "tool", "tool_call_id": callID, "content": output,
		}
	}
	return nil
}

// extractContent 从 responses content（字符串或 parts 数组）提取文本。
// 文本类 part（text/input_text/output_text）取原文；
// 图片类 part（input_image/output_image）降级为 markdown 图片引用（chat 上游为纯文本模型）；
// 音频/文件类 part 降级为链接文本，保证信息不丢。
func extractContent(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			pm, ok := part.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := pm["type"].(string)
			switch typ {
			case "text", "input_text", "output_text":
				if t, ok := pm["text"].(string); ok {
					sb.WriteString(t)
				}
			case "input_image", "output_image":
				if url := imageURL(pm["image_url"]); url != "" {
					sb.WriteString("![image](")
					sb.WriteString(url)
					sb.WriteString(")")
				}
			case "input_audio", "output_audio":
				// 优先 transcript，其次 url
				if t, ok := pm["transcript"].(string); ok && t != "" {
					sb.WriteString("[audio: ")
					sb.WriteString(t)
					sb.WriteString("]")
				} else if u, ok := pm["url"].(string); ok && u != "" {
					sb.WriteString("[audio](")
					sb.WriteString(u)
					sb.WriteString(")")
				}
			case "input_file", "output_file":
				if u, ok := pm["url"].(string); ok && u != "" {
					sb.WriteString("[file](")
					sb.WriteString(u)
					sb.WriteString(")")
				} else if fid, ok := pm["file_id"].(string); ok && fid != "" {
					sb.WriteString("[file: ")
					sb.WriteString(fid)
					sb.WriteString("]")
				}
			default:
				// 未知 part：尝试直接读 text，避免静默丢内容
				if t, ok := pm["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// imageURL 从 image_url 字段（string 或 {url: ...}）提取 URL。
func imageURL(v any) string {
	switch u := v.(type) {
	case string:
		return u
	case map[string]any:
		if s, ok := u["url"].(string); ok {
			return s
		}
	}
	return ""
}
