package server

import (
	"encoding/json"
	"testing"
)

func TestMergeParallelAssistantToolCalls(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"check"},{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},{"role":"assistant","tool_calls":[{"id":"b","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"one"},{"role":"tool","tool_call_id":"b","content":"two"}]}`)
	merged := mergeParallelAssistantToolCalls(body)
	var request map[string]any
	if err := json.Unmarshal(merged, &request); err != nil {
		t.Fatal(err)
	}
	messages, _ := request["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages after merge, got %d: %s", len(messages), merged)
	}
	assistant, _ := messages[1].(map[string]any)
	calls, _ := assistant["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("expected 2 parallel tool calls, got %d: %s", len(calls), merged)
	}
}

func TestInvalidParameterRequestDoesNotQualifyForAccountRetry(t *testing.T) {
	body := []byte(`{"code":11133,"msg":"Invalid request parameters","extError":{"code":"invalid_parameter_value","type":"invalid_request_error"}}`)
	if !isNonRetryableUpstreamRequestError(400, body) {
		t.Fatal("expected invalid parameter response to be non-retryable")
	}
	if isNonRetryableUpstreamRequestError(429, body) {
		t.Fatal("rate-limit response must remain retryable/account-specific")
	}
}
