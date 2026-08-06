package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamResponsesReasoningPassthrough 断言上游 reasoning_content 经 /v1/responses
// 流式出口会被转换为标准的 responses reasoning 事件，且内容完整。
func TestStreamResponsesReasoningPassthrough(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"让我思考一下\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"答案是42\"}}]}\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if err := StreamResponses(rec, strings.NewReader(sse), "hy3"); err != nil {
		t.Fatalf("StreamResponses: %v", err)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"response.reasoning_item.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_item.done",
		`"type":"reasoning"`,
		"让我思考一下",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in stream body:\n%s", want, body)
		}
	}
	// reasoning 项应置于输出数组最前（思考先于回答）
	if idxR := strings.Index(body, `"type":"reasoning"`); idxR >= 0 {
		if idxM := strings.Index(body, `"type":"message"`); idxM >= 0 {
			if idxR > idxM {
				t.Errorf("reasoning item should precede message item in output")
			}
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status=%d", rec.Code)
	}
}

// TestChatToResponseReasoning 断言非流式 /v1/responses 聚合后 reasoning_content
// 以 reasoning 输出项呈现。
func TestChatToResponseReasoning(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"内部推理\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"结论\"}}]}\n\n" +
		"data: [DONE]\n\n"

	chat, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	resp, err := ChatToResponse(chat, "hy3")
	if err != nil {
		t.Fatalf("ChatToResponse: %v", err)
	}
	out := resp["output"].([]any)
	if len(out) < 2 {
		t.Fatalf("expected reasoning + message items, got %d: %v", len(out), out)
	}
	first := out[0].(map[string]any)
	if first["type"] != "reasoning" {
		t.Fatalf("first output should be reasoning, got %v", first["type"])
	}
	summary := first["summary"].([]any)[0].(map[string]any)
	if summary["text"] != "内部推理" {
		t.Errorf("reasoning summary text=%v", summary["text"])
	}
}

// TestStreamResponsesNoReasoning 断言无 reasoning_content 时不应产生 reasoning 事件。
func TestStreamResponsesNoReasoning(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamResponses(rec, strings.NewReader(sse), "glm-5.2"); err != nil {
		t.Fatalf("StreamResponses: %v", err)
	}
	if strings.Contains(rec.Body.String(), "reasoning") {
		t.Errorf("unexpected reasoning events: %s", rec.Body.String())
	}
}
