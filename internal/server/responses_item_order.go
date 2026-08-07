package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// responsesItemOrderingWriter keeps Responses output items sequential. Some
// chat-completions models emit a short assistant message and then start a tool
// call in the same turn. The upstream bridge used to leave that message open
// until the entire tool input had finished streaming, which made ChatGPT.app
// keep showing "thinking" instead of switching to the tool activity card.
type responsesItemOrderingWriter struct {
	http.ResponseWriter
	mu         sync.Mutex
	pending    []byte
	openText   map[string]*openResponseTextItem
	openOrder  []string
	autoClosed map[string]struct{}
}

type openResponseTextItem struct {
	id           string
	outputIndex  int
	contentIndex int
	role         string
	text         strings.Builder
}

func newResponsesItemOrderingWriter(w http.ResponseWriter) http.ResponseWriter {
	return &responsesItemOrderingWriter{
		ResponseWriter: w,
		openText:       map[string]*openResponseTextItem{},
		autoClosed:     map[string]struct{}{},
	}
}

func (w *responsesItemOrderingWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.pending) > 0 || looksLikeSSE(data) {
		w.pending = append(w.pending, data...)
		if err := w.flushCompleteFramesLocked(); err != nil {
			return 0, err
		}
		return len(data), nil
	}
	if _, err := w.ResponseWriter.Write(data); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *responsesItemOrderingWriter) Flush() {
	w.mu.Lock()
	_ = w.flushCompleteFramesLocked()
	w.mu.Unlock()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *responsesItemOrderingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *responsesItemOrderingWriter) flushCompleteFramesLocked() error {
	for {
		index := bytes.Index(w.pending, []byte("\n\n"))
		if index < 0 {
			return nil
		}
		frame := append([]byte(nil), w.pending[:index+2]...)
		w.pending = append(w.pending[:0], w.pending[index+2:]...)
		frames, emit := w.transformFrame(frame)
		if !emit {
			continue
		}
		if _, err := w.ResponseWriter.Write(frames); err != nil {
			return err
		}
	}
}

func (w *responsesItemOrderingWriter) transformFrame(frame []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) {
		return frame, true
	}

	eventName, event, ok := parseResponsesSSEFrame(frame)
	if !ok {
		return frame, true
	}

	switch eventName {
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		itemType, _ := item["type"].(string)
		if itemType == "message" {
			w.rememberOpenMessage(event, item)
			return frame, true
		}
		if itemType == "custom_tool_call" || itemType == "function_call" {
			closures := w.closeOpenMessages()
			if len(closures) == 0 {
				return frame, true
			}
			return append(closures, frame...), true
		}

	case "response.output_text.delta":
		itemID, _ := event["item_id"].(string)
		if item := w.openText[itemID]; item != nil {
			if delta, ok := event["delta"].(string); ok {
				item.text.WriteString(delta)
			}
		}

	case "response.output_text.done", "response.content_part.done":
		itemID, _ := event["item_id"].(string)
		if _, closed := w.autoClosed[itemID]; closed {
			return nil, false
		}
		w.forgetOpenMessage(itemID)

	case "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		if itemType, _ := item["type"].(string); itemType == "message" {
			itemID, _ := item["id"].(string)
			if _, closed := w.autoClosed[itemID]; closed {
				return nil, false
			}
			w.forgetOpenMessage(itemID)
		}
	}
	return frame, true
}

func parseResponsesSSEFrame(frame []byte) (string, map[string]any, bool) {
	lines := strings.Split(strings.TrimSuffix(string(frame), "\n\n"), "\n")
	eventName := ""
	data := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if data == "" {
		return "", nil, false
	}
	var event map[string]any
	if json.Unmarshal([]byte(data), &event) != nil {
		return "", nil, false
	}
	if eventName == "" {
		eventName, _ = event["type"].(string)
	}
	return eventName, event, eventName != ""
}

func (w *responsesItemOrderingWriter) rememberOpenMessage(event, item map[string]any) {
	itemID, _ := item["id"].(string)
	if itemID == "" {
		return
	}
	outputIndex, _ := integerValue(event["output_index"])
	role, _ := item["role"].(string)
	if role == "" {
		role = "assistant"
	}
	if _, exists := w.openText[itemID]; !exists {
		w.openOrder = append(w.openOrder, itemID)
	}
	w.openText[itemID] = &openResponseTextItem{
		id:           itemID,
		outputIndex:  outputIndex,
		contentIndex: 0,
		role:         role,
	}
}

func (w *responsesItemOrderingWriter) closeOpenMessages() []byte {
	var output []byte
	for _, itemID := range w.openOrder {
		item := w.openText[itemID]
		if item == nil {
			continue
		}
		text := item.text.String()
		output = append(output, encodeResponsesSSEEvent("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": item.id,
			"output_index": item.outputIndex, "content_index": item.contentIndex, "text": text,
		})...)
		output = append(output, encodeResponsesSSEEvent("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": item.id,
			"output_index": item.outputIndex, "content_index": item.contentIndex,
			"part": map[string]any{"type": "output_text", "text": text},
		})...)
		output = append(output, encodeResponsesSSEEvent("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": item.outputIndex,
			"item": map[string]any{
				"type": "message", "id": item.id, "status": "completed", "role": item.role,
				"content": []any{map[string]any{"type": "output_text", "text": text}},
			},
		})...)
		w.autoClosed[item.id] = struct{}{}
		delete(w.openText, item.id)
	}
	w.openOrder = w.openOrder[:0]
	return output
}

func (w *responsesItemOrderingWriter) forgetOpenMessage(itemID string) {
	if itemID == "" {
		return
	}
	delete(w.openText, itemID)
	for index, candidate := range w.openOrder {
		if candidate == itemID {
			w.openOrder = append(w.openOrder[:index], w.openOrder[index+1:]...)
			break
		}
	}
}

func encodeResponsesSSEEvent(eventName string, event map[string]any) []byte {
	raw, _ := json.Marshal(event)
	return []byte("event: " + eventName + "\ndata: " + string(raw) + "\n\n")
}
