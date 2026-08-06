// responses_stream.go 将上游 chat completions SSE 流转换为 OpenAI Responses API
// SSE 事件流，供 /v1/responses 端点使用（Codex 兼容）。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// writeSSEEvent 写一条 responses API SSE 事件（event: + data:）。
func writeSSEEvent(w io.Writer, fl http.Flusher, eventType string, data any) {
	raw, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, raw)
	if fl != nil {
		fl.Flush()
	}
}

type toolCallState struct {
	itemID      string
	id          string
	name        string
	arguments   strings.Builder
	started     bool
	outputIndex int
}

// StreamResponses 读取上游 chat completions SSE 流，实时转换为 Responses API SSE 事件。
// model 为透传给客户端的模型名（取自请求）。
func StreamResponses(w http.ResponseWriter, r io.Reader, model string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(r, 64*1024)

	respID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	msgItemID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	var (
		created         = false
		textStarted     = false
		textContent     strings.Builder
		textOutputIndex = -1
		toolItems       = map[int]*toolCallState{}
		toolOrder       []int
		nextOutputIndex = 0
		outputItems     []any
		usage           map[string]any
		// 推理内容（hy3 等思考模型经 delta.reasoning_content 上行；此前被丢弃）
		reasoningStarted     = false
		reasoningContent     strings.Builder
		reasoningOutputIndex = -1
		reasoningItemID      = fmt.Sprintf("rs_%d", time.Now().UnixNano())
	)

	emitCreated := func() {
		if created {
			return
		}
		created = true
		respObj := map[string]any{
			"id":     respID,
			"object": "response",
			"status": "in_progress",
			"model":  model,
			"output": []any{},
		}
		writeSSEEvent(w, fl, "response.created", map[string]any{"type": "response.created", "response": respObj})
		writeSSEEvent(w, fl, "response.in_progress", map[string]any{"type": "response.in_progress", "response": respObj})
	}

	// keepalive 心跳
	stop := make(chan struct{})
	defer close(stop)
	if fl != nil {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-stop:
					return
				case <-t.C:
					io.WriteString(w, ": keepalive\n\n")
					fl.Flush()
				}
			}
		}()
	}

	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				if u, ok := chunk["usage"].(map[string]any); ok {
					usage = u
				}
				if m, ok := chunk["model"].(string); ok && model == "" {
					model = m
				}
				if ch, ok := chunk["choices"].([]any); ok && len(ch) > 0 {
					c, _ := ch[0].(map[string]any)
					if c != nil {
						emitCreated()
						if delta, ok := c["delta"].(map[string]any); ok {
							processDelta(w, fl, delta, &textStarted, &textOutputIndex, &textContent,
								&nextOutputIndex, msgItemID, toolItems, &toolOrder,
								&reasoningStarted, &reasoningOutputIndex, &reasoningContent, reasoningItemID)
						}
					}
				}
			}
		}
		if err != nil {
			break // EOF 或读错误：跳出后优雅收尾
		}
	}

	// 关闭文本输出项
	if textStarted {
		fullText := textContent.String()
		writeSSEEvent(w, fl, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": msgItemID,
			"output_index": textOutputIndex, "content_index": 0, "text": fullText,
		})
		writeSSEEvent(w, fl, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": msgItemID,
			"output_index": textOutputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": fullText},
		})
		writeSSEEvent(w, fl, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": textOutputIndex,
			"item": map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": fullText}},
			},
		})
		outputItems = append(outputItems, map[string]any{
			"type": "message", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": fullText}},
		})
	}

	// 关闭工具调用输出项
	for _, idx := range toolOrder {
		tc := toolItems[idx]
		callID := tc.id
		if callID == "" {
			callID = tc.itemID
		}
		fullArgs := tc.arguments.String()
		writeSSEEvent(w, fl, "response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": callID,
			"output_index": tc.outputIndex, "arguments": fullArgs,
		})
		writeSSEEvent(w, fl, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": tc.outputIndex,
			"item": map[string]any{
				"type": "function_call", "id": callID, "call_id": callID,
				"name": tc.name, "arguments": fullArgs,
			},
		})
		outputItems = append(outputItems, map[string]any{
			"type": "function_call", "id": callID, "call_id": callID,
			"name": tc.name, "arguments": fullArgs,
		})
	}

	// 关闭推理输出项（置于输出数组最前：思考先于回答）
	if reasoningStarted {
		full := reasoningContent.String()
		writeSSEEvent(w, fl, "response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": reasoningItemID,
			"output_index": reasoningOutputIndex, "summary_index": 0, "text": full,
		})
		writeSSEEvent(w, fl, "response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": reasoningItemID,
			"output_index": reasoningOutputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": full},
		})
		reasoningItem := map[string]any{
			"type": "reasoning", "id": reasoningItemID,
			"summary": []any{map[string]any{"type": "summary_text", "text": full}},
			"content": []any{map[string]any{"type": "reasoning_text", "text": full}},
		}
		writeSSEEvent(w, fl, "response.reasoning_item.done", map[string]any{
			"type": "response.reasoning_item.done", "output_index": reasoningOutputIndex, "item": reasoningItem,
		})
		outputItems = append([]any{reasoningItem}, outputItems...)
	}

	// response.completed
	if !created {
		emitCreated()
	}
	if outputItems == nil {
		outputItems = []any{}
	}
	completedResp := map[string]any{
		"id": respID, "object": "response", "status": "completed",
		"model": model, "output": outputItems,
	}
	if usage != nil {
		completedResp["usage"] = usage
	}
	writeSSEEvent(w, fl, "response.completed", map[string]any{
		"type": "response.completed", "response": completedResp,
	})
	return nil
}

// processDelta 处理单个 chat completions delta，发射对应的 responses 事件。
func processDelta(
	w io.Writer, fl http.Flusher, delta map[string]any,
	textStarted *bool, textOutputIndex *int, textContent *strings.Builder,
	nextOutputIndex *int, msgItemID string,
	toolItems map[int]*toolCallState, toolOrder *[]int,
	reasoningStarted *bool, reasoningOutputIndex *int, reasoningContent *strings.Builder, reasoningItemID string,
) {
	// 文本内容
	if txt, ok := delta["content"].(string); ok && txt != "" {
		if !*textStarted {
			*textStarted = true
			*textOutputIndex = *nextOutputIndex
			*nextOutputIndex++
			writeSSEEvent(w, fl, "response.output_item.added", map[string]any{
				"type": "response.output_item.added", "output_index": *textOutputIndex,
				"item": map[string]any{"type": "message", "role": "assistant", "content": []any{}},
			})
			writeSSEEvent(w, fl, "response.content_part.added", map[string]any{
				"type": "response.content_part.added", "item_id": msgItemID,
				"output_index": *textOutputIndex, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": ""},
			})
		}
		textContent.WriteString(txt)
		writeSSEEvent(w, fl, "response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": msgItemID,
			"output_index": *textOutputIndex, "content_index": 0, "delta": txt,
		})
	}

	// 推理内容（hy3 等思考模型的 reasoning_content）→ responses reasoning 事件
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		emitReasoningDelta(w, fl, rc, reasoningStarted, reasoningOutputIndex, reasoningContent, reasoningItemID, nextOutputIndex)
	}

	// 工具调用
	if tcs, ok := delta["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := 0
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			}
			st, seen := toolItems[idx]
			if !seen {
				st = &toolCallState{
					itemID:      fmt.Sprintf("fc_%d_%d", time.Now().UnixNano(), idx),
					outputIndex: *nextOutputIndex,
				}
				*nextOutputIndex++
				toolItems[idx] = st
				*toolOrder = append(*toolOrder, idx)
			}
			if !st.started {
				st.started = true
				if id, ok := call["id"].(string); ok {
					st.id = id
				}
				if fn, ok := call["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok && n != "" {
						st.name = n
					}
				}
				if n, ok := call["name"].(string); ok && n != "" {
					st.name = n
				}
				callID := st.id
				if callID == "" {
					callID = st.itemID
				}
				writeSSEEvent(w, fl, "response.output_item.added", map[string]any{
					"type": "response.output_item.added", "output_index": st.outputIndex,
					"item": map[string]any{
						"type": "function_call", "id": callID, "call_id": callID,
						"name": st.name, "arguments": "",
					},
				})
			}
			if fn, ok := call["function"].(map[string]any); ok {
				if args, ok := fn["arguments"].(string); ok && args != "" {
					st.arguments.WriteString(args)
					writeSSEEvent(w, fl, "response.function_call_arguments.delta", map[string]any{
						"type": "response.function_call_arguments.delta",
						"item_id": st.id, "output_index": st.outputIndex, "delta": args,
					})
				}
			}
		}
	}
}

// emitReasoningDelta 发射单块推理增量，并在首块时初始化 reasoning 输出项。
// 上游仅提供完整思考文本（delta.reasoning_content），无独立 summary；
// 故把同一文本同时作为 summary（用户可见流式通道）与 content（完整思考）承载。
func emitReasoningDelta(w io.Writer, fl http.Flusher, delta string,
	started *bool, outIndex *int, buf *strings.Builder, itemID string, nextOutputIndex *int) {
	if !*started {
		*started = true
		*outIndex = *nextOutputIndex
		*nextOutputIndex++
		writeSSEEvent(w, fl, "response.reasoning_item.added", map[string]any{
			"type":          "response.reasoning_item.added",
			"item_id":       itemID,
			"output_index":  *outIndex,
			"item": map[string]any{"type": "reasoning", "id": itemID, "summary": []any{}, "content": []any{}},
		})
		writeSSEEvent(w, fl, "response.reasoning_summary_part.added", map[string]any{
			"type":         "response.reasoning_summary_part.added",
			"item_id":      itemID,
			"output_index": *outIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	buf.WriteString(delta)
	writeSSEEvent(w, fl, "response.reasoning_summary_text.delta", map[string]any{
		"type":         "response.reasoning_summary_text.delta",
		"item_id":      itemID,
		"output_index": *outIndex, "summary_index": 0, "delta": delta,
	})
}

// AggregateResponse 读取完整 chat completions SSE 流，聚合后转换为 Responses API 响应对象。
func AggregateResponse(r io.Reader, model string) (map[string]any, error) {
	chat, err := Aggregate(r)
	if err != nil {
		return nil, err
	}
	return ChatToResponse(chat, model)
}

// ChatToResponse 将聚合后的 chat completion 对象转换为 Responses API response 对象。
func ChatToResponse(chat map[string]any, model string) (map[string]any, error) {
	respID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	output := []any{}

	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		c, _ := choices[0].(map[string]any)
		if c != nil {
			if msg, ok := c["message"].(map[string]any); ok {
				// 推理内容（hy3 等思考模型）以 reasoning 输出项呈现，置于最前
				if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
					output = append(output, map[string]any{
						"type": "reasoning",
						"id":   fmt.Sprintf("rs_%d", time.Now().UnixNano()),
						"summary": []any{map[string]any{"type": "summary_text", "text": rc}},
						"content": []any{map[string]any{"type": "reasoning_text", "text": rc}},
					})
				}
				if content, ok := msg["content"].(string); ok && content != "" {
					output = append(output, map[string]any{
						"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": content}},
					})
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, _ := tc.(map[string]any)
						if call == nil {
							continue
						}
						id, _ := call["id"].(string)
						fn, _ := call["function"].(map[string]any)
						name, args := "", ""
						if fn != nil {
							name, _ = fn["name"].(string)
							args, _ = fn["arguments"].(string)
						}
						output = append(output, map[string]any{
							"type": "function_call", "id": id, "call_id": id,
							"name": name, "arguments": args,
						})
					}
				}
			}
		}
	}

	resp := map[string]any{
		"id": respID, "object": "response", "status": "completed",
		"model": model, "output": output,
	}
	if usage, ok := chat["usage"]; ok {
		resp["usage"] = usage
	}
	return resp, nil
}
