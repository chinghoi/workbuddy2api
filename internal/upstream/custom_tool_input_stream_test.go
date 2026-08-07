package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeCustomToolInputPrefix(t *testing.T) {
	input := "*** Begin Patch\n+中文 \\\"quoted\\\"\\path\n*** End Patch\n"
	wrapped, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		t.Fatal(err)
	}

	for end := 1; end <= len(wrapped); end++ {
		decoded, complete := DecodeCustomToolInputPrefix(string(wrapped[:end]))
		if !strings.HasPrefix(input, decoded) {
			t.Fatalf("prefix %d decoded non-prefix %q", end, decoded)
		}
		if end < len(wrapped) && complete {
			t.Fatalf("prefix %d reported complete", end)
		}
	}
	decoded, complete := DecodeCustomToolInputPrefix(string(wrapped))
	if !complete || decoded != input {
		t.Fatalf("decoded=%q complete=%v", decoded, complete)
	}
}

func TestStreamResponsesWithToolsStreamsCustomInput(t *testing.T) {
	input := "*** Begin Patch\n" + strings.Repeat("+<path d=\\\"M0 0 L10 10\\\" />\n", 180) + "*** End Patch\n"
	wrapped, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		t.Fatal(err)
	}

	var upstream strings.Builder
	for offset, part := range splitString(string(wrapped), 31) {
		function := map[string]any{"arguments": part}
		call := map[string]any{"index": 0, "function": function}
		if offset == 0 {
			call["id"] = "call_patch"
			function["name"] = "apply_patch"
		}
		chunk := map[string]any{
			"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{call}},
			}},
		}
		raw, _ := json.Marshal(chunk)
		upstream.WriteString("data: ")
		upstream.Write(raw)
		upstream.WriteString("\n\n")
	}
	upstream.WriteString("data: [DONE]\n\n")

	recorder := httptest.NewRecorder()
	err = StreamResponsesWithTools(recorder, strings.NewReader(upstream.String()), "deepseek-v4-flash", ResponseToolMap{
		"apply_patch": {Kind: ResponseToolCustom, Name: "apply_patch"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var deltas []string
	var doneInput string
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.custom_tool_call_input.delta":
			delta, _ := event["delta"].(string)
			deltas = append(deltas, delta)
		case "response.custom_tool_call_input.done":
			doneInput, _ = event["input"].(string)
		}
	}
	if len(deltas) < 2 {
		t.Fatalf("expected multiple realtime deltas, got %d", len(deltas))
	}
	if got := strings.Join(deltas, ""); got != input {
		t.Fatalf("streamed input mismatch: got %d bytes want %d", len(got), len(input))
	}
	if doneInput != input {
		t.Fatalf("done input mismatch: got %d bytes want %d", len(doneInput), len(input))
	}
}

func splitString(value string, size int) []string {
	var parts []string
	for len(value) > 0 {
		end := size
		if end > len(value) {
			end = len(value)
		}
		parts = append(parts, value[:end])
		value = value[end:]
	}
	return parts
}
