package upstream

import (
	"net/http/httptest"
	"strings"
	"testing"
)

const customToolSSE = "data: {\"model\":\"glm-5.2\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"exec\",\"arguments\":\"{\\\"input\\\":\\\"text(\\\\\\\"ok\\\\\\\")\\\"}\"}}]}}]}\n\ndata: [DONE]\n\n"

func TestStreamResponsesWithToolsRestoresCustomCall(t *testing.T) {
	recorder := httptest.NewRecorder()
	err := StreamResponsesWithTools(recorder, strings.NewReader(customToolSSE), "glm-5.2", ResponseToolMap{
		"exec": {Kind: ResponseToolCustom, Name: "exec"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, want := range []string{"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", `"type":"custom_tool_call"`, `text(\"ok\")`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "response.function_call_arguments.done") {
		t.Fatalf("custom call leaked as function: %s", body)
	}
}

func TestAggregateResponseWithToolsRestoresCustomCall(t *testing.T) {
	response, err := AggregateResponseWithTools(strings.NewReader(customToolSSE), "glm-5.2", ResponseToolMap{
		"exec": {Kind: ResponseToolCustom, Name: "exec"},
	})
	if err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%#v", output)
	}
	item := output[0].(map[string]any)
	if item["type"] != "custom_tool_call" || item["input"] != `text("ok")` {
		t.Fatalf("item=%#v", item)
	}
}
