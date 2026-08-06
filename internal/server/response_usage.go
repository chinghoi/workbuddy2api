package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// responsesUsageWriter normalizes token usage in completed Responses API
// payloads. WorkBuddy emits Chat Completions usage fields such as
// prompt_tokens/completion_tokens, while Responses clients require
// input_tokens/output_tokens and the corresponding detail objects.
type responsesUsageWriter struct {
	http.ResponseWriter
}

func newResponsesUsageWriter(w http.ResponseWriter) http.ResponseWriter {
	return &responsesUsageWriter{ResponseWriter: w}
}

func (w *responsesUsageWriter) Write(data []byte) (int, error) {
	normalized := normalizeResponsesUsageChunk(data)
	_, err := w.ResponseWriter.Write(normalized)
	if err != nil {
		return 0, err
	}
	// Report the original input length to callers such as fmt.Fprintf even when
	// normalization changes the serialized byte length.
	return len(data), nil
}

func (w *responsesUsageWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responsesUsageWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func normalizeResponsesUsageChunk(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	// Non-stream Responses JSON is written in one chunk.
	var object map[string]any
	if json.Unmarshal(data, &object) == nil && normalizeCompletedResponseObject(object) {
		if normalized, err := json.Marshal(object); err == nil {
			return normalized
		}
	}

	// Streaming events are emitted as one event/data block by writeSSEEvent.
	text := string(data)
	lines := strings.Split(text, "\n")
	changed := false
	for index, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		if !normalizeCompletedResponseObject(event) {
			continue
		}
		normalized, err := json.Marshal(event)
		if err != nil {
			continue
		}
		lines[index] = "data: " + string(normalized)
		changed = true
	}
	if changed {
		return []byte(strings.Join(lines, "\n"))
	}
	return data
}

func normalizeCompletedResponseObject(object map[string]any) bool {
	if eventType, _ := object["type"].(string); eventType == "response.completed" {
		response, _ := object["response"].(map[string]any)
		if response == nil {
			return false
		}
		normalizeResponseUsage(response)
		return true
	}
	if objectType, _ := object["object"].(string); objectType == "response" {
		if status, _ := object["status"].(string); status == "completed" {
			normalizeResponseUsage(object)
			return true
		}
	}
	return false
}

func normalizeResponseUsage(response map[string]any) {
	usage, _ := response["usage"].(map[string]any)
	if usage == nil {
		usage = map[string]any{}
	}

	inputTokens := firstTokenNumber(usage["input_tokens"], usage["prompt_tokens"])
	outputTokens := firstTokenNumber(usage["output_tokens"], usage["completion_tokens"])
	totalTokens, hasTotal := tokenNumber(usage["total_tokens"])
	if !hasTotal {
		totalTokens = inputTokens + outputTokens
	}

	inputDetails, _ := usage["input_tokens_details"].(map[string]any)
	if inputDetails == nil {
		inputDetails = map[string]any{}
	}
	if _, exists := inputDetails["cached_tokens"]; !exists {
		cachedTokens := float64(0)
		if promptDetails, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			cachedTokens, _ = tokenNumber(promptDetails["cached_tokens"])
		}
		inputDetails["cached_tokens"] = cachedTokens
	}

	outputDetails, _ := usage["output_tokens_details"].(map[string]any)
	if outputDetails == nil {
		outputDetails = map[string]any{}
	}
	if _, exists := outputDetails["reasoning_tokens"]; !exists {
		reasoningTokens := float64(0)
		if completionDetails, ok := usage["completion_tokens_details"].(map[string]any); ok {
			reasoningTokens, _ = tokenNumber(completionDetails["reasoning_tokens"])
		}
		outputDetails["reasoning_tokens"] = reasoningTokens
	}

	usage["input_tokens"] = inputTokens
	usage["input_tokens_details"] = inputDetails
	usage["output_tokens"] = outputTokens
	usage["output_tokens_details"] = outputDetails
	usage["total_tokens"] = totalTokens
	response["usage"] = usage
}

func firstTokenNumber(values ...any) float64 {
	for _, value := range values {
		if number, ok := tokenNumber(value); ok {
			return number
		}
	}
	return 0
}

func tokenNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
