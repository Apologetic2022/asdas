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
		[]ChatMessage{{
			Role:       "tool",
			Name:       "Write",
			ToolCallID: "toolu_fable_write",
			Content:    "file created",
		}},
		[]ToolDefinition{{Name: "Write"}},
		ToolChoice{},
	)
	if err != nil {
		t.Fatal(err)
	}
	run := message.GetRunRequest()
	if run.GetAction().GetResumeAction() == nil {
		t.Fatalf("tool-result continuation must use native ResumeAction: %+v", run.GetAction())
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
	if run.GetAction().GetResumeAction() == nil {
		t.Fatal("completed native tool turn must continue with ResumeAction")
	}
	if len(run.GetConversationState().GetTurns()) != 1 {
		t.Fatalf("native turns = %d, want 1", len(run.GetConversationState().GetTurns()))
	}
	if got := nativeToolResultsFromState(t, run.GetConversationState(), blobs)["toolu_append"]; got != "ok" {
		t.Fatalf("appended native result = %q", got)
	}
}
