package cursor

import (
	"strings"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func writeToolResolver(name string) *ToolDefinition {
	tools := []ToolDefinition{{Name: "Write"}}
	return indexTools(tools)[name]
}

func TestTextToolCallFilterConvertsFableMarkerAcrossDeltas(t *testing.T) {
	filter := &textToolCallFilter{}
	chunks := []string{
		"I will create it. [cal",
		"led tool mcp_Wri",
		`te id=toolu_01Wr3pWkq6Yq93aaSeEjXV8w arguments={"file_path":"outputs/组件兼容性说明.html","contents":"<div data-x=\"]\">ok</div>"}`,
		"] Done.",
	}
	var visible strings.Builder
	var calls []ToolCall
	for _, chunk := range chunks {
		for _, output := range filter.rewrite(chunk, writeToolResolver) {
			if output.call != nil {
				calls = append(calls, *output.call)
			} else {
				visible.WriteString(output.text)
			}
		}
	}
	visible.WriteString(filter.flush())

	if strings.Contains(visible.String(), "[called tool") || strings.Contains(visible.String(), "mcp_Write") {
		t.Fatalf("tool marker leaked into visible text: %q", visible.String())
	}
	if got := visible.String(); got != "I will create it.  Done." {
		t.Fatalf("visible text = %q", got)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.Name != "Write" || call.ID != "toolu_01Wr3pWkq6Yq93aaSeEjXV8w" {
		t.Fatalf("converted call = %+v", call)
	}
	if call.Arguments["file_path"] != "outputs/组件兼容性说明.html" {
		t.Fatalf("file_path = %#v", call.Arguments["file_path"])
	}
	if call.Arguments["contents"] != `<div data-x="]">ok</div>` {
		t.Fatalf("contents = %#v", call.Arguments["contents"])
	}
}

func TestTextToolCallFilterPreservesUnknownOrIncompleteExamples(t *testing.T) {
	filter := &textToolCallFilter{}
	unknown := `[called tool mcp_Delete id=example arguments={"path":"x"}]`
	outputs := filter.rewrite(unknown, writeToolResolver)
	var visible strings.Builder
	for _, output := range outputs {
		if output.call != nil {
			t.Fatalf("unknown marker became a call: %+v", output.call)
		}
		visible.WriteString(output.text)
	}
	visible.WriteString(filter.flush())
	if visible.String() != unknown {
		t.Fatalf("unknown marker changed to %q", visible.String())
	}

	filter = &textToolCallFilter{}
	outputs = filter.rewrite("literal [called to", writeToolResolver)
	visible.Reset()
	for _, output := range outputs {
		visible.WriteString(output.text)
	}
	visible.WriteString(filter.flush())
	if visible.String() != "literal [called to" {
		t.Fatalf("incomplete example changed to %q", visible.String())
	}
}

func TestSessionTurnsTextualWriteIntoToolCallEvent(t *testing.T) {
	input := `[called tool mcp_Write id=toolu_fable arguments={"file_path":"outputs/page.html","contents":"ok"}]`
	inputTokens := int64(100)
	outputTokens := int64(10)
	session := &Session{
		ID:             "session-fable",
		Model:          "claude-fable-5",
		tools:          []ToolDefinition{{Name: "Write"}},
		toolIndex:      indexTools([]ToolDefinition{{Name: "Write"}}),
		events:         make(chan StreamEvent, 16),
		errCh:          make(chan error, 1),
		textToolFilter: &textToolCallFilter{},
		promptTokens:   inputTokens,
		checkpoint:     testConversationState(),
		ckptAfterEnd:   true,
		blobStore:      map[string][]byte{},
	}

	_, err := session.handleServerMessage(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TextDelta{
					TextDelta: &agentv1.TextDeltaUpdate{Text: input},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.handleServerMessage(&agentv1.AgentServerMessage{
		Message: &agentv1.AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &agentv1.InteractionUpdate{
				Message: &agentv1.InteractionUpdate_TurnEnded{
					TurnEnded: &agentv1.TurnEndedUpdate{
						InputTokens:  &inputTokens,
						OutputTokens: &outputTokens,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var visible strings.Builder
	var calls []ToolCall
	reason := ""
	for len(session.events) > 0 {
		event := <-session.events
		switch event.Type {
		case "text_delta":
			visible.WriteString(event.Text)
		case "tool_call":
			if event.ToolCall != nil {
				calls = append(calls, *event.ToolCall)
			}
		case "segment_end":
			reason = event.Reason
		}
	}
	if visible.Len() != 0 {
		t.Fatalf("textual tool marker leaked as %q", visible.String())
	}
	if len(calls) != 1 || calls[0].Name != "Write" {
		t.Fatalf("tool calls = %+v", calls)
	}
	if reason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", reason)
	}
}
