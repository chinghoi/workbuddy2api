package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// TestResponsesToChatBodyPassThroughFields 断言新增透传字段原样进入 chat 请求体。
func TestResponsesToChatBodyPassThroughFields(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4-flash",
		"input":"hi",
		"stream":true,
		"max_output_tokens":64,
		"n":2,
		"logit_bias":{"1234":-1},
		"response_format":{"type":"json_object"},
		"stream_options":{"include_usage":true},
		"metadata":{"session":"abc"},
		"temperature":0.7
	}`)
	out := responsesToChatBody(body)
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("parse: %v", err)
	}
	cases := []struct {
		key  string
		want any
	}{
		{"n", float64(2)},
		{"stream_options", map[string]any{"include_usage": true}},
		{"metadata", map[string]any{"session": "abc"}},
		{"response_format", map[string]any{"type": "json_object"}},
		{"logit_bias", map[string]any{"1234": float64(-1)}},
		{"max_tokens", float64(64)},
		{"temperature", 0.7},
	}
	for _, c := range cases {
		got, ok := req[c.key]
		if !ok {
			t.Errorf("key %s missing", c.key)
			continue
		}
		// 用 JSON 重编码做深度比较（map 键序无关）
		gw, _ := json.Marshal(got)
		ww, _ := json.Marshal(c.want)
		if string(gw) != string(ww) {
			t.Errorf("%s = %s, want %s", c.key, gw, ww)
		}
	}
	// 模型名原样透传
	if req["model"] != "deepseek-v4-flash" {
		t.Errorf("model=%v", req["model"])
	}
}

// TestResponsesToChatBodyDoesNotForceStream 断言转换层不再硬编码 stream（由 PrepareBody 统一强制）。
func TestResponsesToChatBodyDoesNotForceStream(t *testing.T) {
	out := responsesToChatBody([]byte(`{"model":"m","input":"hi"}`))
	var req map[string]any
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, present := req["stream"]; present {
		t.Errorf("stream should not be set by responsesToChatBody: %v", req)
	}
}

// TestExtractContentImageAudioFile 断言图片/音频/文件 part 降级为文本引用。
func TestExtractContentImageAudioFile(t *testing.T) {
	content := []any{
		map[string]any{"type": "input_text", "text": "看看这张图："},
		map[string]any{"type": "input_image", "image_url": map[string]any{"url": "https://img.example.com/a.png"}},
		map[string]any{"type": "output_image", "image_url": "https://img.example.com/b.png"},
		map[string]any{"type": "input_audio", "transcript": "你好"},
		map[string]any{"type": "output_audio", "url": "https://a.example.com/x.mp3"},
		map[string]any{"type": "input_file", "file_id": "file_123"},
		map[string]any{"type": "unknown_part", "text": "fallback"},
	}
	got := extractContent(content)
	for _, want := range []string{
		"看看这张图：",
		"![image](https://img.example.com/a.png)",
		"![image](https://img.example.com/b.png)",
		"[audio: 你好]",
		"[audio](https://a.example.com/x.mp3)",
		"[file: file_123]",
		"fallback",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("extractContent missing %q: %q", want, got)
		}
	}
}

// TestExtractContentStringAndPlainParts 回归：纯文本场景行为不变。
func TestExtractContentStringAndPlainParts(t *testing.T) {
	if got := extractContent("plain"); got != "plain" {
		t.Errorf("string input: %q", got)
	}
	if got := extractContent(nil); got != "" {
		t.Errorf("nil input: %q", got)
	}
	if got := extractContent([]any{map[string]any{"type": "output_text", "text": "a"}}); got != "a" {
		t.Errorf("output_text: %q", got)
	}
}

// TestResponsesNonStreamAggregates 断言非流式 /v1/responses 请求在
// 上游（强制流式）返回 SSE 时聚合为完整 JSON response 对象。
func TestResponsesNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true // 上游总是流式
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	ct := rec.Header().Get("Content-Type")
	if strings.Contains(ct, "event-stream") {
		t.Fatalf("non-stream request should return JSON, got SSE: ct=%q body=%s", ct, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("resp object/status: %v / %v", resp["object"], resp["status"])
	}
	out := resp["output"].([]any)
	if len(out) == 0 {
		t.Fatalf("no output items: %s", rec.Body)
	}
	first := out[0].(map[string]any)
	if first["type"] != "message" {
		t.Errorf("first output type=%v", first["type"])
	}
}

// TestResponsesStreamPassthrough 断言流式 /v1/responses 请求直通 SSE。
func TestResponsesStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Errorf("missing response.completed: %q", body)
	}
	if !strings.Contains(body, `"type":"response.output_text.delta"`) {
		t.Errorf("missing output_text.delta: %q", body)
	}
}
