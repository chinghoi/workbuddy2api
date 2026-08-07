package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactResponsesToolOutputsOmitsBrowserBase64(t *testing.T) {
	base64Run := strings.Repeat("A", 4096)
	body, err := json.Marshal(map[string]any{
		"model": "hy3",
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": "inspect the page",
				}},
			},
			map[string]any{
				"type":    "custom_tool_call_output",
				"call_id": "call_1",
				"output": []any{map[string]any{
					"type": "input_text",
					"text": `{"content":[{"type":"text","text":"title: example"},{"type":"image","data":"` + base64Run + `"}],"_meta":{"screenshot":{"url":"data:image/jpeg;base64,` + base64Run + `"}}}`,
				}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	compacted := compactResponsesToolOutputs(body)
	if strings.Contains(string(compacted), base64Run) {
		t.Fatal("base64 image payload remained in compacted request")
	}
	if !strings.Contains(string(compacted), omittedBinaryPayloadMarker) {
		t.Fatalf("missing compaction marker: %s", compacted)
	}
	if !strings.Contains(string(compacted), "title: example") {
		t.Fatalf("useful text was removed: %s", compacted)
	}
	if !strings.Contains(string(compacted), "inspect the page") {
		t.Fatalf("unrelated user content changed: %s", compacted)
	}
}

func TestCompactResponsesToolOutputsLeavesPlainTextUntouched(t *testing.T) {
	body := []byte(`{"model":"hy3","input":[{"type":"custom_tool_call_output","call_id":"call_1","output":[{"type":"input_text","text":"plain tool result"}]}]}`)
	if got := compactResponsesToolOutputs(body); string(got) != string(body) {
		t.Fatalf("plain tool output changed: %s", got)
	}
}

func TestReplaceLongBase64RunsHonorsBoundaryAndPadding(t *testing.T) {
	short := strings.Repeat("A", minimumBase64RunLength-1)
	if got, changed := replaceLongBase64Runs(short, minimumBase64RunLength); changed || got != short {
		t.Fatalf("short run changed: changed=%v len=%d", changed, len(got))
	}

	long := "prefix," + strings.Repeat("B", minimumBase64RunLength) + "==,suffix"
	got, changed := replaceLongBase64Runs(long, minimumBase64RunLength)
	if !changed {
		t.Fatal("expected long base64 run to be replaced")
	}
	want := "prefix," + omittedBinaryPayloadMarker + ",suffix"
	if got != want {
		t.Fatalf("unexpected replacement:\n got: %q\nwant: %q", got, want)
	}
}
