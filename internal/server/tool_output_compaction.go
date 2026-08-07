package server

import (
	"encoding/json"
	"regexp"
	"strings"
)

const omittedBinaryPayloadMarker = "[binary image omitted from upstream model context; retained in data/full_io]"

// Browser tool results can contain screenshots twice: once as a content image
// and again as a data: URL in _meta. In the Responses request these values are
// embedded inside input_text JSON, so the chat-completions upstream cannot see
// them as images; it only pays the token cost of the base64. Keep complete raw
// request/output capture on disk, but remove those opaque runs from the body
// sent to the language model.
var longBase64RunPattern = regexp.MustCompile(`[A-Za-z0-9+/]{2048,}={0,2}`)

func compactResponsesToolOutputs(body []byte) []byte {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return body
	}
	input, ok := request["input"].([]any)
	if !ok {
		return body
	}

	changed := false
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		if typ != "custom_tool_call_output" && typ != "function_call_output" {
			continue
		}
		output, exists := item["output"]
		if !exists {
			continue
		}
		compacted, outputChanged := compactToolOutputValue(output, false)
		if outputChanged {
			item["output"] = compacted
			changed = true
		}
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return encoded
}

func compactToolOutputValue(value any, imageContext bool) (any, bool) {
	switch typed := value.(type) {
	case string:
		return compactToolOutputString(typed, imageContext)
	case []any:
		changed := false
		out := make([]any, len(typed))
		for index, child := range typed {
			compacted, childChanged := compactToolOutputValue(child, imageContext)
			out[index] = compacted
			changed = changed || childChanged
		}
		return out, changed
	case map[string]any:
		localImageContext := imageContext
		if typ, _ := typed["type"].(string); typ == "image" || typ == "input_image" || typ == "output_image" {
			localImageContext = true
		}
		changed := false
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			childImageContext := localImageContext || key == "screenshot" || key == "image" || key == "image_url" || key == "data"
			if key == "url" {
				if text, ok := child.(string); ok && strings.HasPrefix(text, "data:image/") {
					childImageContext = true
				}
			}
			compacted, childChanged := compactToolOutputValue(child, childImageContext)
			out[key] = compacted
			changed = changed || childChanged
		}
		return out, changed
	default:
		return value, false
	}
}

func compactToolOutputString(value string, imageContext bool) (string, bool) {
	containsImageEnvelope := strings.Contains(value, "data:image/") ||
		strings.Contains(value, `"type": "image"`) ||
		strings.Contains(value, `"screenshot"`)
	if !imageContext && !containsImageEnvelope {
		return value, false
	}

	replaced := false
	compacted := longBase64RunPattern.ReplaceAllStringFunc(value, func(string) string {
		replaced = true
		return omittedBinaryPayloadMarker
	})
	return compacted, replaced
}
