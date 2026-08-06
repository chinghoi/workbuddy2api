package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// responsesUsageWriter applies the remaining Responses API compatibility
// rules at the final client-output boundary:
//   - normalize Chat Completions token usage fields;
//   - add a monotonically increasing sequence_number to every SSE event;
//   - suppress raw upstream reasoning_content, which is not a reasoning summary;
//   - close output-index gaps left by suppressed reasoning items.
//
// The wrapper sits outside the full-I/O capture writer, so output.raw records
// the exact payload delivered to the desktop client.
type responsesUsageWriter struct {
	http.ResponseWriter
	mu               sync.Mutex
	pending          []byte
	nextSequence     int64
	reasoningIndexes map[int]struct{}
}

func newResponsesUsageWriter(w http.ResponseWriter) http.ResponseWriter {
	return &responsesUsageWriter{
		ResponseWriter:   w,
		nextSequence:     1,
		reasoningIndexes: map[int]struct{}{},
	}
}

func (w *responsesUsageWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.pending) > 0 || looksLikeSSE(data) {
		w.pending = append(w.pending, data...)
		if err := w.flushCompleteSSEFramesLocked(); err != nil {
			return 0, err
		}
		return len(data), nil
	}

	normalized := normalizeResponsesUsageChunk(data)
	_, err := w.ResponseWriter.Write(normalized)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *responsesUsageWriter) Flush() {
	w.mu.Lock()
	_ = w.flushCompleteSSEFramesLocked()
	w.mu.Unlock()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responsesUsageWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func looksLikeSSE(data []byte) bool {
	trimmed := bytes.TrimLeft(data, "\r\n\t ")
	return bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte(":"))
}

func (w *responsesUsageWriter) flushCompleteSSEFramesLocked() error {
	for {
		index := bytes.Index(w.pending, []byte("\n\n"))
		if index < 0 {
			return nil
		}
		frame := append([]byte(nil), w.pending[:index+2]...)
		w.pending = append(w.pending[:0], w.pending[index+2:]...)
		transformed, emit := w.transformSSEFrame(frame)
		if !emit {
			continue
		}
		if _, err := w.ResponseWriter.Write(transformed); err != nil {
			return err
		}
	}
}

func (w *responsesUsageWriter) transformSSEFrame(frame []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) {
		return frame, true
	}

	lines := strings.Split(strings.TrimSuffix(string(frame), "\n\n"), "\n")
	eventName := ""
	dataIndex := -1
	for index, line := range lines {
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			dataIndex = index
		}
	}
	if dataIndex < 0 {
		return frame, true
	}

	var event map[string]any
	if json.Unmarshal([]byte(strings.TrimPrefix(lines[dataIndex], "data: ")), &event) != nil {
		return frame, true
	}
	if eventName == "" {
		eventName, _ = event["type"].(string)
	}

	if strings.HasPrefix(eventName, "response.reasoning_") {
		w.rememberReasoningIndex(event)
		return nil, false
	}
	if item, _ := event["item"].(map[string]any); item != nil {
		if itemType, _ := item["type"].(string); itemType == "reasoning" {
			w.rememberReasoningIndex(event)
			return nil, false
		}
	}

	w.remapEventOutputIndex(event)
	if eventName == "response.completed" {
		normalizeCompletedResponseObject(event)
	}
	event["sequence_number"] = w.nextSequence
	w.nextSequence++

	raw, err := json.Marshal(event)
	if err != nil {
		return frame, true
	}
	lines[dataIndex] = "data: " + string(raw)
	return []byte(strings.Join(lines, "\n") + "\n\n"), true
}

func (w *responsesUsageWriter) rememberReasoningIndex(event map[string]any) {
	if index, ok := integerValue(event["output_index"]); ok {
		w.reasoningIndexes[index] = struct{}{}
	}
}

func (w *responsesUsageWriter) remapEventOutputIndex(event map[string]any) {
	index, ok := integerValue(event["output_index"])
	if !ok {
		return
	}
	shift := 0
	for reasoningIndex := range w.reasoningIndexes {
		if reasoningIndex < index {
			shift++
		}
	}
	event["output_index"] = index - shift
}

func integerValue(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	case int64:
		return int(number), true
	case json.Number:
		parsed, err := number.Int64()
		return int(parsed), err == nil
	default:
		return 0, false
	}
}

func normalizeResponsesUsageChunk(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	var object map[string]any
	if json.Unmarshal(data, &object) == nil && normalizeCompletedResponseObject(object) {
		if normalized, err := json.Marshal(object); err == nil {
			return normalized
		}
	}

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
		normalizeResponseObject(response)
		return true
	}
	if objectType, _ := object["object"].(string); objectType == "response" {
		if status, _ := object["status"].(string); status == "completed" {
			normalizeResponseObject(object)
			return true
		}
	}
	return false
}

func normalizeResponseObject(response map[string]any) {
	normalizeResponseUsage(response)
	if output, ok := response["output"].([]any); ok {
		filtered := make([]any, 0, len(output))
		for _, rawItem := range output {
			item, _ := rawItem.(map[string]any)
			if itemType, _ := item["type"].(string); itemType == "reasoning" {
				continue
			}
			filtered = append(filtered, rawItem)
		}
		response["output"] = filtered
	}
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

// Keep sort imported by older downstream builds that patch this file together
// with generated compatibility shims. Remove when development captures are
// retired and the bridge surface is frozen.
var _ = sort.Ints
