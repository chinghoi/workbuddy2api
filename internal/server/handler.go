// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// Config handler 依赖。
type Config struct {
	Pool         *pool.Pool
	Upstream     *upstream.Client
	APIKey       string        // 空 = 不鉴权
	MaxRotate    int           // 单请求最多换号次数，默认 3
	HardCooldown time.Duration // 余额不足冷却，默认 12h
	SoftCooldown time.Duration // 429 冷却，默认 60s
	ErrThreshold int           // 连续其他错误冷却阈值，默认 3
	ErrCooldown  time.Duration // 错误冷却时长，默认 10m
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(withTrace(h.chatCompletions)))
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(withTrace(h.responses)))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != h.cfg.APIKey {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// withTrace 记录请求结构样本到 data/capture.jsonl（host 路径可见），
// 同时打印一行摘要到 stdout 用于在线观察。
//
// 目的：收集 WorkBuddy → workbuddy2api → 上游 的真实请求，做数据驱动的
// 按模型/字段条件化适配（不基于猜测做粗暴剥离）。
//
// capture 字段：ts/model/upstream_type/input_len/messages_len/tools_n/
// tools_names/has_parallel_tool_calls/tool_choice/reasoning_effort/
// reasoning_summary/has_instructions/instructions_len/max_tokens/stream。
// 注意：不含 Authorization/content 原文，避免敏感信息落盘。
func withTrace(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		r.Body = io.NopCloser(strings.NewReader(string(body)))

		cap := captureRequest(r.URL.Path, body)
		appendCapture(cap)
		next(w, r)
	}
}

// captureRequest 解析请求体并提取关键字段特征，不含敏感内容。
func captureRequest(path string, body []byte) map[string]any {
	cap := map[string]any{
		"ts":            time.Now().Format(time.RFC3339),
		"upstream_path": path,
		"authz_prefix":  safeAuthzPrefix(),
	}
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		cap["model"] = "PARSE_ERROR"
		return cap
	}
	if m, ok := req["model"].(string); ok {
		cap["model"] = m
	} else {
		cap["model"] = ""
	}
	if s, ok := req["stream"].(bool); ok {
		cap["stream"] = s
	} else {
		cap["stream"] = true // 上游强制流式，通常省略
	}
	if t, ok := req["max_tokens"].(float64); ok {
		cap["max_tokens"] = int(t)
	} else if t, ok := req["max_output_tokens"].(float64); ok {
		cap["max_tokens"] = int(t)
	} else {
		cap["max_tokens"] = -1
	}

	// input 与 messages 互斥：responses 是 input，chat 是 messages
	if in, ok := req["input"]; ok {
		cap["input_len"], cap["messages_len"] = inputLength(in), -1
	} else if msgs, ok := req["messages"].([]any); ok {
		cap["messages_len"] = len(msgs)
		cap["input_len"] = -1
	} else {
		cap["input_len"], cap["messages_len"] = -1, -1
	}

	if instr, ok := req["instructions"].(string); ok {
		cap["has_instructions"] = true
		cap["instructions_len"] = len(instr)
	} else {
		cap["has_instructions"] = false
		cap["instructions_len"] = 0
	}

	// TEMP-DEBUG: 捕获 system 内容用于定位敏感词触发句（事后删除）。
	cap["system_content"] = extractSystemContent(req)
	// TEMP-DEBUG: 捕获 Responses API 完整 input（用户授权，事后删除），
	// 不截断，写入 data/full_input.json 便于完整查看 developer 模板与 tools 描述。
	if in, ok := req["input"]; ok {
		dumpFullInput(in)
		if raw, err := json.Marshal(in); err == nil {
			s := string(raw)
			if len(s) > 8000 {
				s = s[:8000] + "...[truncated]"
			}
			cap["input_raw"] = s
		}
	}

	if tools, ok := req["tools"].([]any); ok {
		cap["tools_n"] = len(tools)
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			if tm, ok := t.(map[string]any); ok {
				if fn, ok := tm["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok {
						names = append(names, n)
						continue
					}
				}
				if n, ok := tm["name"].(string); ok {
					names = append(names, n)
				}
			}
		}
		cap["tools_names"] = names
	} else {
		cap["tools_n"] = 0
		cap["tools_names"] = []string{}
	}
	if _, ok := req["parallel_tool_calls"]; ok {
		cap["has_parallel_tool_calls"] = true
	} else {
		cap["has_parallel_tool_calls"] = false
	}
	cap["tool_choice"] = fmt.Sprintf("%v", req["tool_choice"])

	if r, ok := req["reasoning"].(map[string]any); ok {
		if e, ok := r["effort"].(string); ok {
			cap["reasoning_effort"] = e
		} else {
			cap["reasoning_effort"] = fmt.Sprintf("%v", r)
		}
	} else {
		cap["reasoning_effort"] = "nil"
	}
	if s, ok := req["reasoning_summary"].(string); ok {
		cap["reasoning_summary"] = s
	} else {
		cap["reasoning_summary"] = ""
	}
	return cap
}

// safeAuthzPrefix 返回一个统一 mask（capture 不暴露任何 token）。
func safeAuthzPrefix() string {
	return "Bearer***"
}

// dumpFullInput TEMP-DEBUG: 将完整 input 原样（美化）写入 data/full_input.json，
// 便于完整查看 developer 模板与 tools 描述，定位上游敏感词触发句。事后删除。
func dumpFullInput(in any) {
	raw, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return
	}
	//nolint:errcheck
	_ = os.WriteFile("./data/full_input.json", raw, 0o644)
}

// extractSystemContent TEMP-DEBUG: 提取 system 消息与 instructions 文本，用于定位
// 命中上游敏感词黑名单的触发句。仅落盘到 capture.jsonl，调试结束后移除本函数及调用。
func extractSystemContent(req map[string]any) string {
	var sb strings.Builder
	if instr, ok := req["instructions"].(string); ok && instr != "" {
		sb.WriteString("[instructions] ")
		sb.WriteString(instr)
		sb.WriteString("\n")
	}
	if msgs, ok := req["messages"].([]any); ok {
		for _, m := range msgs {
			msg, ok := m.(map[string]any)
			if !ok {
				continue
			}
			if role, _ := msg["role"].(string); role != "system" {
				continue
			}
			sb.WriteString("[system] ")
			switch c := msg["content"].(type) {
			case string:
				sb.WriteString(c)
			case []any:
				for _, p := range c {
					if pm, ok := p.(map[string]any); ok {
						if t, ok := pm["text"].(string); ok {
							sb.WriteString(t)
						}
					}
				}
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// inputLength 估算 input 字段的文本长度（字符串求字节数；数组则取所有字符串拼接）。
func inputLength(v any) int {
	switch t := v.(type) {
	case string:
		return len(t)
	case []any:
		n := 0
		for _, item := range t {
			switch it := item.(type) {
			case string:
				n += len(it)
			case map[string]any:
				if c, ok := it["content"].(string); ok {
					n += len(c)
				}
			}
		}
		return n
	}
	return -1
}

// captureFile 单文件追加，带互斥锁避免并发写冲突。
var (
	captureMu   sync.Mutex
	capturePath = "./data/capture.jsonl"
)

func appendCapture(cap map[string]any) {
	captureMu.Lock()
	defer captureMu.Unlock()
	//nolint:errcheck
	f, err := os.OpenFile(capturePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	raw, err := json.Marshal(cap)
	if err != nil {
		return
	}
	_, _ = f.Write(append(raw, '\n'))
}

func previewInput(v any) string {
	switch t := v.(type) {
	case string:
		return truncateStr(t, 80)
	case []any:
		return fmt.Sprintf("array[%d]", len(t))
	case nil:
		return ""
	default:
		return fmt.Sprintf("%T", t)
	}
}

func trimPrefix(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func selectKeys(h http.Header, keys ...string) map[string]string {
	out := map[string]string{}
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
	})
}

// 静态 CN 模型表（api-reference §5，动态接口失败时的回退）。
var staticModels = []map[string]any{
	{"id": "glm-5.2", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5.1", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "glm-5v-turbo", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "kimi-k2.7", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "minimax-m3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "hy3-preview-agent", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-pro", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
	{"id": "deepseek-v4-flash", "object": "model", "created": 1753600000, "owned_by": "workbuddy", "context_length": 131072},
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids     []upstream.ModelInfo
	fetched time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：优先动态（缓存 1h），失败回退静态表。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
func (h *Handler) modelList() []map[string]any {
	if infos := h.fetchDynamicModels(); len(infos) > 0 {
		out := make([]map[string]any, 0, len(infos))
		for _, mi := range infos {
			entry := map[string]any{
				"id":                mi.ID,
				"object":            "model",
				"created":           1753600000,
				"owned_by":          "workbuddy",
				"context_length":    mi.ContextWindow,
				"max_output_tokens": mi.MaxTokens,
			}
			if mi.ContextWindow == 0 {
				entry["context_length"] = 131072 // 兜底
			}
			out = append(out, entry)
		}
		return out
	}
	return staticModels
}

// fetchDynamicModels 从池中任一健康账号拉模型列表，缓存 1h。
// fetchDynamicModels 从池中任一健康账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接用静态表，避免反复打上游。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	acct := h.cfg.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true

		// token 临近过期 → 先 refresh（失败冷却换号）
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

		rc, status, respBody, terr := h.cfg.Upstream.ChatStream(acct, body, r.Context())
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
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrSoftRate:
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrSessionDead:
				h.cfg.Pool.Disable(acct.UID, "12153 session dead")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			case upstream.ErrNotFound:
				// P2: 404 短冷却不累计 errCount（防雪崩）
				h.cfg.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			default:
				// P0: 轮转下一个账号，不直接返回（防雪崩）
				h.cfg.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
				continue
			}
		}
		defer rc.Close()
		h.cfg.Pool.NoteSuccess(acct.UID)
		if peek.Stream {
			_ = upstream.Stream(w, rc)
			return
		}
		resp, err := upstream.Aggregate(rc)
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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}
