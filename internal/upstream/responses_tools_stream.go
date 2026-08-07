package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type bridgedToolCallState struct {
	itemID          string
	callID          string
	name            string
	arguments       strings.Builder
	customPending   strings.Builder
	customDecoded   int
	started         bool
	custom          bool
	outputIndex     int
}

// StreamResponsesWithTools converts the WorkBuddy chat-completions SSE stream
// to Responses API events while restoring custom tools that were compiled into
// synthetic function tools on the request path.
func StreamResponsesWithTools(w http.ResponseWriter, r io.Reader, model string, tools ResponseToolMap) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReaderSize(r, 64*1024)

	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	reasoningID := fmt.Sprintf("rs_%d", time.Now().UnixNano())

	created := false
	textStarted := false
	textOutputIndex := -1
	var text strings.Builder
	reasoningStarted := false
	reasoningOutputIndex := -1
	var reasoning strings.Builder
	toolStates := map[int]*bridgedToolCallState{}
	toolOrder := []int{}
	nextOutputIndex := 0
	outputItems := []any{}
	var usage map[string]any

	emitCreated := func() {
		if created {
			return
		}
		created = true
		response := map[string]any{
			"id": responseID, "object": "response", "status": "in_progress",
			"model": model, "output": []any{},
		}
		writeSSEEvent(w, flusher, "response.created", map[string]any{"type": "response.created", "response": response})
		writeSSEEvent(w, flusher, "response.in_progress", map[string]any{"type": "response.in_progress", "response": response})
	}

	stopKeepalive := make(chan struct{})
	var keepaliveWG sync.WaitGroup
	defer func() {
		close(stopKeepalive)
		keepaliveWG.Wait()
	}()
	if flusher != nil {
		keepaliveWG.Add(1)
		go func() {
			defer keepaliveWG.Done()
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopKeepalive:
					return
				case <-ticker.C:
					_, _ = io.WriteString(w, ": keepalive\n\n")
					flusher.Flush()
				}
			}
		}()
	}

	for {
		line, readErr := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data: ") {
			payload := strings.TrimPrefix(trimmed, "data: ")
			if payload == "[DONE]" {
				break
			}
			var chunk map[string]any
			if json.Unmarshal([]byte(payload), &chunk) == nil {
				if chunkUsage, ok := chunk["usage"].(map[string]any); ok {
					usage = chunkUsage
				}
				if upstreamModel, ok := chunk["model"].(string); ok && model == "" {
					model = upstreamModel
				}
				choices, _ := chunk["choices"].([]any)
				if len(choices) > 0 {
					choice, _ := choices[0].(map[string]any)
					delta, _ := choice["delta"].(map[string]any)
					if delta != nil {
						emitCreated()
						if value, ok := delta["content"].(string); ok && value != "" {
							if !textStarted {
								textStarted = true
								textOutputIndex = nextOutputIndex
								nextOutputIndex++
								writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
									"type": "response.output_item.added", "output_index": textOutputIndex,
									"item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "content": []any{}},
								})
								writeSSEEvent(w, flusher, "response.content_part.added", map[string]any{
									"type": "response.content_part.added", "item_id": messageID,
									"output_index": textOutputIndex, "content_index": 0,
									"part": map[string]any{"type": "output_text", "text": ""},
								})
							}
							text.WriteString(value)
							writeSSEEvent(w, flusher, "response.output_text.delta", map[string]any{
								"type": "response.output_text.delta", "item_id": messageID,
								"output_index": textOutputIndex, "content_index": 0, "delta": value,
							})
						}
						if value, ok := delta["reasoning_content"].(string); ok && value != "" {
							if !reasoningStarted {
								reasoningStarted = true
								reasoningOutputIndex = nextOutputIndex
								nextOutputIndex++
								writeSSEEvent(w, flusher, "response.reasoning_item.added", map[string]any{
									"type": "response.reasoning_item.added", "item_id": reasoningID,
									"output_index": reasoningOutputIndex,
									"item":         map[string]any{"type": "reasoning", "id": reasoningID, "summary": []any{}, "content": []any{}},
								})
								writeSSEEvent(w, flusher, "response.reasoning_summary_part.added", map[string]any{
									"type": "response.reasoning_summary_part.added", "item_id": reasoningID,
									"output_index": reasoningOutputIndex, "summary_index": 0,
									"part": map[string]any{"type": "summary_text", "text": ""},
								})
							}
							reasoning.WriteString(value)
							writeSSEEvent(w, flusher, "response.reasoning_summary_text.delta", map[string]any{
								"type": "response.reasoning_summary_text.delta", "item_id": reasoningID,
								"output_index": reasoningOutputIndex, "summary_index": 0, "delta": value,
							})
						}
						if calls, ok := delta["tool_calls"].([]any); ok {
							for _, rawCall := range calls {
								call, ok := rawCall.(map[string]any)
								if !ok {
									continue
								}
								index := 0
								if rawIndex, ok := call["index"].(float64); ok {
									index = int(rawIndex)
								}
								state, exists := toolStates[index]
								if !exists {
									state = &bridgedToolCallState{
										itemID:      fmt.Sprintf("fc_%d_%d", time.Now().UnixNano(), index),
										outputIndex: nextOutputIndex,
									}
									nextOutputIndex++
									toolStates[index] = state
									toolOrder = append(toolOrder, index)
								}
								if id, ok := call["id"].(string); ok && id != "" {
									state.callID = id
								}
								function, _ := call["function"].(map[string]any)
								if function != nil {
									if name, ok := function["name"].(string); ok && name != "" {
										state.name = name
										state.custom = tools.IsCustom(name)
									}
								}
								if name, ok := call["name"].(string); ok && name != "" {
									state.name = name
									state.custom = tools.IsCustom(name)
								}
								if !state.started && state.name != "" {
									emitBridgedToolAdded(w, flusher, state)
								}
								if function != nil {
									if arguments, ok := function["arguments"].(string); ok && arguments != "" {
										state.arguments.WriteString(arguments)
										if state.started && state.custom {
											streamBridgedCustomToolInput(w, flusher, state, false)
										} else if state.started {
											writeSSEEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
												"type": "response.function_call_arguments.delta", "item_id": state.itemID,
												"output_index": state.outputIndex, "delta": arguments,
											})
										}
									}
								}
							}
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return readErr
			}
			break
		}
	}

	if reasoningStarted {
		full := reasoning.String()
		writeSSEEvent(w, flusher, "response.reasoning_summary_text.done", map[string]any{
			"type": "response.reasoning_summary_text.done", "item_id": reasoningID,
			"output_index": reasoningOutputIndex, "summary_index": 0, "text": full,
		})
		writeSSEEvent(w, flusher, "response.reasoning_summary_part.done", map[string]any{
			"type": "response.reasoning_summary_part.done", "item_id": reasoningID,
			"output_index": reasoningOutputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": full},
		})
		item := map[string]any{
			"type": "reasoning", "id": reasoningID,
			"summary": []any{map[string]any{"type": "summary_text", "text": full}},
			"content": []any{map[string]any{"type": "reasoning_text", "text": full}},
		}
		writeSSEEvent(w, flusher, "response.reasoning_item.done", map[string]any{
			"type": "response.reasoning_item.done", "output_index": reasoningOutputIndex, "item": item,
		})
		outputItems = append(outputItems, item)
	}

	if textStarted {
		full := text.String()
		writeSSEEvent(w, flusher, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": messageID,
			"output_index": textOutputIndex, "content_index": 0, "text": full,
		})
		writeSSEEvent(w, flusher, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": messageID,
			"output_index": textOutputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full},
		})
		item := map[string]any{
			"type": "message", "id": messageID, "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": full}},
		}
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": textOutputIndex, "item": item,
		})
		outputItems = append(outputItems, item)
	}

	for _, index := range toolOrder {
		state := toolStates[index]
		if !state.started {
			emitBridgedToolAdded(w, flusher, state)
		}
		outputItems = append(outputItems, finishBridgedTool(w, flusher, state))
	}

	if !created {
		emitCreated()
	}
	completed := map[string]any{
		"id": responseID, "object": "response", "status": "completed",
		"model": model, "output": outputItems,
	}
	if usage != nil {
		completed["usage"] = usage
	}
	writeSSEEvent(w, flusher, "response.completed", map[string]any{"type": "response.completed", "response": completed})
	return nil
}

func emitBridgedToolAdded(w io.Writer, flusher http.Flusher, state *bridgedToolCallState) {
	state.started = true
	if state.callID == "" {
		state.callID = state.itemID
	}
	item := map[string]any{
		"id": state.itemID, "call_id": state.callID, "name": state.name,
	}
	if state.custom {
		item["type"] = "custom_tool_call"
		item["input"] = ""
	} else {
		item["type"] = "function_call"
		item["arguments"] = ""
	}
	writeSSEEvent(w, flusher, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": state.outputIndex, "item": item,
	})
}

func streamBridgedCustomToolInput(w io.Writer, flusher http.Flusher, state *bridgedToolCallState, force bool) {
	decoded, _ := DecodeCustomToolInputPrefix(state.arguments.String())
	if len(decoded) > state.customDecoded {
		state.customPending.WriteString(decoded[state.customDecoded:])
		state.customDecoded = len(decoded)
	}
	if state.customPending.Len() < customToolInputFlushBytes && !force {
		return
	}
	if state.customPending.Len() == 0 {
		return
	}
	delta := state.customPending.String()
	state.customPending.Reset()
	writeSSEEvent(w, flusher, "response.custom_tool_call_input.delta", map[string]any{
		"type": "response.custom_tool_call_input.delta", "item_id": state.itemID,
		"output_index": state.outputIndex, "delta": delta,
	})
}

func finishBridgedTool(w io.Writer, flusher http.Flusher, state *bridgedToolCallState) map[string]any {
	if state.callID == "" {
		state.callID = state.itemID
	}
	arguments := state.arguments.String()
	if state.custom {
		input := DecodeCustomToolInput(arguments)
		if len(input) > state.customDecoded {
			state.customPending.WriteString(input[state.customDecoded:])
			state.customDecoded = len(input)
		}
		streamBridgedCustomToolInput(w, flusher, state, true)
		writeSSEEvent(w, flusher, "response.custom_tool_call_input.done", map[string]any{
			"type": "response.custom_tool_call_input.done", "item_id": state.itemID,
			"output_index": state.outputIndex, "input": input,
		})
		item := map[string]any{
			"type": "custom_tool_call", "id": state.itemID, "call_id": state.callID,
			"name": state.name, "input": input,
		}
		writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": state.outputIndex, "item": item,
		})
		return item
	}
	writeSSEEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "item_id": state.itemID,
		"output_index": state.outputIndex, "arguments": arguments,
	})
	item := map[string]any{
		"type": "function_call", "id": state.itemID, "call_id": state.callID,
		"name": state.name, "arguments": arguments,
	}
	writeSSEEvent(w, flusher, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": state.outputIndex, "item": item,
	})
	return item
}

func AggregateResponseWithTools(r io.Reader, model string, tools ResponseToolMap) (map[string]any, error) {
	chat, err := Aggregate(r)
	if err != nil {
		return nil, err
	}
	return ChatToResponseWithTools(chat, model, tools)
}

func ChatToResponseWithTools(chat map[string]any, model string, tools ResponseToolMap) (map[string]any, error) {
	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	output := []any{}
	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message != nil {
			if value, ok := message["reasoning_content"].(string); ok && value != "" {
				output = append(output, map[string]any{
					"type": "reasoning", "id": fmt.Sprintf("rs_%d", time.Now().UnixNano()),
					"summary": []any{map[string]any{"type": "summary_text", "text": value}},
					"content": []any{map[string]any{"type": "reasoning_text", "text": value}},
				})
			}
			if value, ok := message["content"].(string); ok && value != "" {
				output = append(output, map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": value}},
				})
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, _ := rawCall.(map[string]any)
					if call == nil {
						continue
					}
					callID, _ := call["id"].(string)
					function, _ := call["function"].(map[string]any)
					name, arguments := "", ""
					if function != nil {
						name, _ = function["name"].(string)
						arguments, _ = function["arguments"].(string)
					}
					itemID := fmt.Sprintf("fc_%d", time.Now().UnixNano())
					if tools.IsCustom(name) {
						output = append(output, map[string]any{
							"type": "custom_tool_call", "id": itemID, "call_id": callID,
							"name": name, "input": DecodeCustomToolInput(arguments),
						})
					} else {
						output = append(output, map[string]any{
							"type": "function_call", "id": itemID, "call_id": callID,
							"name": name, "arguments": arguments,
						})
					}
			}
		}
	}
	response := map[string]any{
		"id": responseID, "object": "response", "status": "completed",
		"model": model, "output": output,
	}
	if usage, ok := chat["usage"]; ok {
		response["usage"] = usage
	}
	return response, nil
}
