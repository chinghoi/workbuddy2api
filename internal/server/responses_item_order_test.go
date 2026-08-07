package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOrderingWriterClosesMessageBeforeCustomTool(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := newResponsesItemOrderingWriter(recorder)

	frames := []string{
		testSSE("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 0,
			"item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{}},
		}),
		testSSE("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": "msg_1", "output_index": 0, "content_index": 0, "delta": "正在修改文件",
		}),
		testSSE("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "output_index": 1,
			"item": map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1", "name": "apply_patch", "input": ""},
		}),
		testSSE("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": "msg_1", "output_index": 0, "content_index": 0, "text": "正在修改文件",
		}),
		testSSE("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": "msg_1", "output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "正在修改文件"},
		}),
		testSSE("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{"type": "message", "id": "msg_1", "role": "assistant"},
		}),
	}
	for _, frame := range frames {
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}

	output := recorder.Body.String()
	messageDone := strings.Index(output, "event: response.output_item.done")
	toolAdded := strings.Index(output, `"type":"custom_tool_call"`)
	if messageDone < 0 || toolAdded < 0 || messageDone > toolAdded {
		t.Fatalf("message was not closed before tool item:\n%s", output)
	}
	if count := strings.Count(output, "event: response.output_text.done"); count != 1 {
		t.Fatalf("output_text.done count=%d want 1\n%s", count, output)
	}
	if count := strings.Count(output, "event: response.content_part.done"); count != 1 {
		t.Fatalf("content_part.done count=%d want 1\n%s", count, output)
	}
	if count := strings.Count(output, "event: response.output_item.done"); count != 1 {
		t.Fatalf("message output_item.done count=%d want 1\n%s", count, output)
	}
}

func testSSE(name string, event map[string]any) string {
	raw, _ := json.Marshal(event)
	return "event: " + name + "\ndata: " + string(raw) + "\n\n"
}
