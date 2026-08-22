package cursor

import (
	"encoding/json"
	"strings"
)

const (
	textToolCallPrefix  = "[called tool "
	maxTextToolCallSize = 8 << 20
)

type textToolCallOutput struct {
	text string
	call *ToolCall
}

// textToolCallFilter recognizes the pseudo-tool syntax that some Cursor
// models (fable in particular) emit as prose:
//
//	[called tool mcp_Write id=toolu_... arguments={...}]
//
// That syntax is also present in older replay prompts, so the model can learn
// to imitate it instead of issuing an MCP exec. Deltas may split anywhere,
// including inside the prefix or JSON. The filter holds only the undecidable
// suffix, passes ordinary text through, and converts a complete marker only
// when its name resolves to a tool declared by this client.
type textToolCallFilter struct {
	pending string
}

func (f *textToolCallFilter) rewrite(chunk string, resolve func(string) *ToolDefinition) []textToolCallOutput {
	if f == nil {
		if chunk == "" {
			return nil
		}
		return []textToolCallOutput{{text: chunk}}
	}
	input := f.pending + chunk
	f.pending = ""
	outputs := make([]textToolCallOutput, 0, 2)
	appendText := func(text string) {
		if text == "" {
			return
		}
		if len(outputs) > 0 && outputs[len(outputs)-1].call == nil {
			outputs[len(outputs)-1].text += text
			return
		}
		outputs = append(outputs, textToolCallOutput{text: text})
	}

	for input != "" {
		index := strings.Index(input, textToolCallPrefix)
		if index < 0 {
			hold := trailingTextToolPrefixLen(input)
			appendText(input[:len(input)-hold])
			f.pending = input[len(input)-hold:]
			break
		}
		appendText(input[:index])
		input = input[index:]

		call, consumed, status := parseTextToolCall(input)
		switch status {
		case textToolCallIncomplete:
			if len(input) <= maxTextToolCallSize {
				f.pending = input
				return outputs
			}
			// An unbounded unterminated marker is not a tool call. Release one
			// byte and continue scanning instead of retaining attacker-sized
			// output forever.
			appendText(input[:1])
			input = input[1:]
		case textToolCallInvalid:
			appendText(input[:1])
			input = input[1:]
		case textToolCallComplete:
			definition := resolve(call.Name)
			if definition == nil {
				// It may be a literal example or a tool owned by somebody
				// else. Preserve it exactly unless this session advertised it.
				appendText(input[:consumed])
			} else {
				call.Name = definition.Name
				outputs = append(outputs, textToolCallOutput{call: call})
			}
			input = input[consumed:]
		}
	}
	return outputs
}

func (f *textToolCallFilter) flush() string {
	if f == nil {
		return ""
	}
	text := f.pending
	f.pending = ""
	return text
}

type textToolCallParseStatus uint8

const (
	textToolCallIncomplete textToolCallParseStatus = iota
	textToolCallInvalid
	textToolCallComplete
)

func parseTextToolCall(input string) (*ToolCall, int, textToolCallParseStatus) {
	if !strings.HasPrefix(input, textToolCallPrefix) {
		return nil, 0, textToolCallInvalid
	}
	nameStart := len(textToolCallPrefix)
	nameEndRelative := strings.Index(input[nameStart:], " id=")
	if nameEndRelative < 0 {
		return nil, 0, markerFieldStatus(input)
	}
	nameEnd := nameStart + nameEndRelative
	name := strings.TrimSpace(input[nameStart:nameEnd])
	if name == "" || len(name) > 256 {
		return nil, 0, textToolCallInvalid
	}

	idStart := nameEnd + len(" id=")
	idEndRelative := strings.Index(input[idStart:], " arguments=")
	if idEndRelative < 0 {
		return nil, 0, markerFieldStatus(input)
	}
	idEnd := idStart + idEndRelative
	callID := strings.TrimSpace(input[idStart:idEnd])
	if callID == "" || len(callID) > 1024 {
		return nil, 0, textToolCallInvalid
	}

	argumentsStart := idEnd + len(" arguments=")
	argumentsEnd, status := scanJSONObject(input, argumentsStart)
	if status != textToolCallComplete {
		return nil, 0, status
	}
	closing := argumentsEnd
	for closing < len(input) && (input[closing] == ' ' || input[closing] == '\t' || input[closing] == '\r' || input[closing] == '\n') {
		closing++
	}
	if closing >= len(input) {
		return nil, 0, textToolCallIncomplete
	}
	if input[closing] != ']' {
		return nil, 0, textToolCallInvalid
	}

	arguments := map[string]any{}
	if err := json.Unmarshal([]byte(input[argumentsStart:argumentsEnd]), &arguments); err != nil {
		return nil, 0, textToolCallInvalid
	}
	return &ToolCall{
		ID:        callID,
		Name:      name,
		Arguments: arguments,
	}, closing + 1, textToolCallComplete
}

func markerFieldStatus(input string) textToolCallParseStatus {
	if len(input) > maxTextToolCallSize || strings.Contains(input, "]") {
		return textToolCallInvalid
	}
	return textToolCallIncomplete
}

// scanJSONObject returns the byte immediately after the JSON object. Brackets
// inside quoted HTML/file contents do not terminate the marker.
func scanJSONObject(input string, start int) (int, textToolCallParseStatus) {
	if start >= len(input) {
		return 0, textToolCallIncomplete
	}
	if input[start] != '{' {
		return 0, textToolCallInvalid
	}
	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(input); index++ {
		char := input[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return index + 1, textToolCallComplete
			}
			if depth < 0 {
				return 0, textToolCallInvalid
			}
		}
	}
	return 0, textToolCallIncomplete
}

func trailingTextToolPrefixLen(input string) int {
	longest := len(textToolCallPrefix) - 1
	if longest > len(input) {
		longest = len(input)
	}
	for length := longest; length > 0; length-- {
		if strings.HasPrefix(textToolCallPrefix, input[len(input)-length:]) {
			return length
		}
	}
	return 0
}
