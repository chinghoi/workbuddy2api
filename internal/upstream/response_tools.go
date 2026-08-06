package upstream

import (
	"encoding/json"
	"strings"
)

// ResponseToolKind records the original Responses API tool type before it is
// compiled into a chat-completions function tool for the WorkBuddy upstream.
type ResponseToolKind string

const (
	ResponseToolFunction ResponseToolKind = "function"
	ResponseToolCustom   ResponseToolKind = "custom"
)

// ResponseToolSpec is the minimum metadata needed to restore an upstream
// function call to the Responses API item type expected by the client.
type ResponseToolSpec struct {
	Kind ResponseToolKind
	Name string
}

// ResponseToolMap is keyed by the tool name sent to WorkBuddy.
type ResponseToolMap map[string]ResponseToolSpec

func (m ResponseToolMap) IsCustom(name string) bool {
	if m == nil {
		return false
	}
	spec, ok := m[name]
	return ok && spec.Kind == ResponseToolCustom
}

// DecodeCustomToolInput unwraps the synthetic {"input":"..."} arguments used
// to carry a Responses custom tool through the chat-completions upstream.
// It deliberately falls back to the original argument string so a malformed
// model response is still observable by the client instead of being dropped.
func DecodeCustomToolInput(arguments string) string {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(arguments), &obj); err == nil {
		for _, key := range []string{"input", "code", "text"} {
			if value, ok := obj[key].(string); ok {
				return value
			}
		}
	}
	return arguments
}
