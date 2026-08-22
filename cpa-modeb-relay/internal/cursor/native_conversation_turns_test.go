package cursor

import (
	"encoding/hex"
	"strings"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func nativeToolResultsFromState(t *testing.T, state *agentv1.ConversationStateStructure, blobs map[string][]byte) map[string]string {
	t.Helper()
	results := make(map[string]string)
	for _, turnID := range state.GetTurns() {
		turn := &agentv1.ConversationTurnStructure{}
		if err := proto.Unmarshal(blobs[hex.EncodeToString(turnID)], turn); err != nil {
			t.Fatalf("decode turn: %v", err)
		}
		for _, stepID := range turn.GetAgentConversationTurn().GetSteps() {
			step := &agentv1.ConversationStep{}
			if err := proto.Unmarshal(blobs[hex.EncodeToString(stepID)], step); err != nil {
				t.Fatalf("decode step: %v", err)
			}
			call := step.GetToolCall()
			if call == nil || call.GetMcpToolCall() == nil || call.GetMcpToolCall().GetResult() == nil {
				continue
			}
			content := call.GetMcpToolCall().GetResult().GetSuccess().GetContent()
			if len(content) > 0 {
				results[call.GetToolCallId()] = content[0].GetText().GetText()
			}
		}
	}
	return results
}

func TestNativeTailResumePatchesToolResultWithoutTextReplay(t *testing.T) {
	blobs := map[string][]byte{}
	turns, _, err := buildNativeConversationTurns(blobs, []ChatMessage{
		{Role: "user", Content: "write the file"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID:   "toolu_fable_write",
			Name: "Write",
			Arguments: map[string]any{
				"file_path": "outputs/page.html",
				"contents":  "<html>ok</html>",
			},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry := &convEntry{
		conversationID: "conv-native",
		state:          &agentv1.ConversationStateStructure{Turns: turns},
		blobs:          blobs,
		model:          "claude-fable-5",
	}

	message, replayBlobs, _, err := buildNativeTailResumeRequest(
		"claude-fable-5",
		entry,
		[]ChatMessage{
			{
				Role:       "tool",
				Name:       "Write",
				ToolCallID: "toolu_fable_write",
				Content:    "file created",
			},
			{Role: "system", Content: "<total_tokens>15000000 tokens left</total_tokens>"},
		},
		[]ToolDefinition{{Name: "Write"}},
		ToolChoice{},
	)
	if err != nil {
		t.Fatal(err)
	}
	run := message.GetRunRequest()
	if got := run.GetAction().GetUserMessageAction().GetUserMessage().GetText(); !strings.HasPrefix(got, resumeContinuationPrompt) {
		t.Fatalf("tool-result continuation action = %q", got)
	}
	if got := nativeToolResultsFromState(t, run.GetConversationState(), replayBlobs)["toolu_fable_write"]; got != "file created" {
		t.Fatalf("native tool result = %q", got)
	}
	for _, payload := range replayBlobs {
		if strings.Contains(string(payload), "[called tool") {
			t.Fatal("native replay blobs contain the textual pseudo-tool marker")
		}
	}
}

func TestNativeTailResumeAppendsCompletedToolTurn(t *testing.T) {
	entry := &convEntry{
		conversationID: "conv-native-append",
		state:          &agentv1.ConversationStateStructure{},
		blobs:          map[string][]byte{},
		model:          "claude-fable-5",
	}
	tail := []ChatMessage{
		{Role: "user", Content: "create a page"},
		{Role: "assistant", Content: "I will write it.", ToolCalls: []ToolCall{{
			ID:   "toolu_append",
			Name: "Write",
			Arguments: map[string]any{
				"file_path": "outputs/page.html",
				"contents":  "<html>native</html>",
			},
		}}},
		{Role: "tool", Name: "Write", ToolCallID: "toolu_append", Content: "ok"},
	}
	message, blobs, _, err := buildNativeTailResumeRequest(
		"claude-fable-5", entry, tail, []ToolDefinition{{Name: "Write"}}, ToolChoice{},
	)
	if err != nil {
		t.Fatal(err)
	}
	run := message.GetRunRequest()
	if got := run.GetAction().GetUserMessageAction().GetUserMessage().GetText(); !strings.HasPrefix(got, resumeContinuationPrompt) {
		t.Fatalf("completed native tool turn continuation = %q", got)
	}
	if len(run.GetConversationState().GetTurns()) != 1 {
		t.Fatalf("native turns = %d, want 1", len(run.GetConversationState().GetTurns()))
	}
	if got := nativeToolResultsFromState(t, run.GetConversationState(), blobs)["toolu_append"]; got != "ok" {
		t.Fatalf("appended native result = %q", got)
	}
}

func TestTrailingSystemReminderStaysInCurrentUserAction(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: "stable system prompt"},
		{Role: "user", Content: "old question"},
		{Role: "assistant", Content: "old answer"},
		{Role: "user", Content: "current question"},
		{Role: "system", Content: "current system reminder"},
	}
	prefix, turn := splitTrailingUserRun(messages)
	if len(prefix) != 3 || len(turn) != 2 {
		t.Fatalf("prefix=%d turn=%d, want 3/2", len(prefix), len(turn))
	}
	message, _, _, err := buildRunRequest("claude-opus-5", messages, nil, ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	action := message.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if action != "current question\n\ncurrent system reminder" {
		t.Fatalf("action = %q", action)
	}
	if action == resumeContinuationPrompt {
		t.Fatal("trailing system reminder was mistaken for a tool continuation")
	}
}

func TestSynthesizeConversationCheckpointWhenCursorOmitsUpdate(t *testing.T) {
	state, blobs, err := synthesizeConversationCheckpoint(
		&agentv1.ConversationStateStructure{},
		map[string][]byte{},
		"current question\n\ncurrent system reminder",
		[]ChatMessage{{Role: "assistant", Content: "completed answer"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.GetTurns()) != 1 {
		t.Fatalf("turns = %d, want 1", len(state.GetTurns()))
	}
	turn := &agentv1.ConversationTurnStructure{}
	if err = proto.Unmarshal(blobs[hex.EncodeToString(state.GetTurns()[0])], turn); err != nil {
		t.Fatal(err)
	}
	user := &agentv1.UserMessage{}
	if err = proto.Unmarshal(blobs[hex.EncodeToString(turn.GetAgentConversationTurn().GetUserMessage())], user); err != nil {
		t.Fatal(err)
	}
	if user.GetText() != "current question\n\ncurrent system reminder" {
		t.Fatalf("native user action = %q", user.GetText())
	}
	steps := turn.GetAgentConversationTurn().GetSteps()
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(steps))
	}
	step := &agentv1.ConversationStep{}
	if err = proto.Unmarshal(blobs[hex.EncodeToString(steps[0])], step); err != nil {
		t.Fatal(err)
	}
	if got := step.GetAssistantMessage().GetText(); got != "completed answer" {
		t.Fatalf("assistant step = %q", got)
	}
}
