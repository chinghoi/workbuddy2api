package upstream

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"
)

const customToolInputFlushBytes = 256

// DecodeCustomToolInputPrefix decodes the complete prefix of the synthetic
// {"input":"..."} wrapper while the JSON string is still arriving in stream
// fragments. An incomplete trailing escape is held until the next fragment.
// The returned text is always valid UTF-8 and can safely be emitted as a
// response.custom_tool_call_input.delta.
func DecodeCustomToolInputPrefix(arguments string) (decoded string, complete bool) {
	var completeObject map[string]any
	if json.Unmarshal([]byte(arguments), &completeObject) == nil {
		if input, ok := completeObject["input"].(string); ok {
			return input, true
		}
	}

	keyIndex := strings.Index(arguments, `"input"`)
	if keyIndex < 0 {
		return "", false
	}
	index := keyIndex + len(`"input"`)
	for index < len(arguments) && isJSONSpace(arguments[index]) {
		index++
	}
	if index >= len(arguments) || arguments[index] != ':' {
		return "", false
	}
	index++
	for index < len(arguments) && isJSONSpace(arguments[index]) {
		index++
	}
	if index >= len(arguments) || arguments[index] != '"' {
		return "", false
	}
	index++

	var out strings.Builder
	for index < len(arguments) {
		value := arguments[index]
		if value == '"' {
			return out.String(), true
		}
		if value != '\\' {
			out.WriteByte(value)
			index++
			continue
		}

		if index+1 >= len(arguments) {
			break
		}
		escape := arguments[index+1]
		switch escape {
		case '"', '\\', '/':
			out.WriteByte(escape)
			index += 2
		case 'b':
			out.WriteByte('\b')
			index += 2
		case 'f':
			out.WriteByte('\f')
			index += 2
		case 'n':
			out.WriteByte('\n')
			index += 2
		case 'r':
			out.WriteByte('\r')
			index += 2
		case 't':
			out.WriteByte('\t')
			index += 2
		case 'u':
			first, next, ok := decodeJSONUnicodeEscape(arguments, index)
			if !ok {
				return out.String(), false
			}
			index = next
			if utf16.IsSurrogate(first) {
				second, afterSecond, secondOK := decodeJSONUnicodeEscape(arguments, index)
				if !secondOK {
					return out.String(), false
				}
				decodedRune := utf16.DecodeRune(first, second)
				if decodedRune == '\uFFFD' {
					out.WriteRune(first)
					continue
				}
				out.WriteRune(decodedRune)
				index = afterSecond
				continue
			}
			out.WriteRune(first)
		default:
			// Invalid JSON escapes are left for the final full decoder/fallback.
			return out.String(), false
		}
	}
	return out.String(), false
}

func decodeJSONUnicodeEscape(value string, index int) (rune, int, bool) {
	if index+6 > len(value) || value[index] != '\\' || value[index+1] != 'u' {
		return 0, index, false
	}
	parsed, err := strconv.ParseUint(value[index+2:index+6], 16, 16)
	if err != nil {
		return 0, index, false
	}
	return rune(parsed), index + 6, true
}

func isJSONSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}
