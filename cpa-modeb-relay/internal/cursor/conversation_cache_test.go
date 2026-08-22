package cursor

import (
	"testing"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
)

func testConversationState() *agentv1.ConversationStateStructure {
	return &agentv1.ConversationStateStructure{Turns: [][]byte{[]byte("turn")}}
}

func TestConversationStateTurnsUsesAuditedFieldNumber(t *testing.T) {
	field := (&agentv1.ConversationStateStructure{}).ProtoReflect().Descriptor().Fields().ByName("turns")
	if field == nil || field.Number() != 8 {
		t.Fatalf("ConversationStateStructure.turns field = %v, want 8", field)
	}
}

func TestConversationFingerprintTracksSystemPromptAndNormalizesToolIDs(t *testing.T) {
	stored := []ChatMessage{
		{Role: "system", Content: "be concise"},
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call-1\npart", Name: "list", Arguments: map[string]any{"path": "/tmp"},
		}}},
		{Role: "tool", ToolCallID: "call-1_part", Content: "a.txt"},
	}
	echoed := []ChatMessage{
		{Role: "system", Content: "be concise"},
		{Role: "user", Content: "list files"},
		{Role: "assistant", ToolCalls: []ToolCall{{
			ID: "call-1_part", Name: "list", Arguments: map[string]any{"path": "/tmp"},
		}}},
		{Role: "tool", ToolCallID: "call-1_part", Content: "a.txt"},
	}
	if conversationFingerprint(stored) != conversationFingerprint(echoed) {
		t.Fatal("tool id normalization must preserve the echoed transcript fingerprint")
	}
	changed := append([]ChatMessage(nil), echoed...)
	changed[0].Content = "be verbose"
	if conversationFingerprint(changed) == conversationFingerprint(echoed) {
		t.Fatal("changing the system prompt must invalidate the checkpoint fingerprint")
	}
}

func TestConversationCacheScopesOpusCheckpointByResolvedModel(t *testing.T) {
	t.Setenv("CPA_CURSOR_CONV_CACHE_DIR", t.TempDir())
	account := "auth:test-account"
	opus := ResolveRequestedModel("claude-5-opus").ModelID
	if opus != "claude-opus-5" {
		t.Fatalf("resolved opus model = %q", opus)
	}
	fp := conversationFingerprint([]ChatMessage{{Role: "user", Content: "hello"}})
	opusScope := convScope(account, opus)
	otherScope := convScope(account, "claude-sonnet-4-6")
	defaultConversationCache.Store(opusScope, fp, &convEntry{
		conversationID: "conv-opus",
		state:          testConversationState(),
		model:          opus,
	})
	t.Cleanup(func() { defaultConversationCache.Invalidate(opusScope, fp) })

	if entry, ok := defaultConversationCache.Lookup(opusScope, fp); !ok || entry.conversationID != "conv-opus" {
		t.Fatal("opus checkpoint was not found under its resolved wire model")
	}
	if _, ok := defaultConversationCache.Lookup(otherScope, fp); ok {
		t.Fatal("a checkpoint must not leak into another model's scope")
	}
}

func TestBoundaryLookupWaitsForTrailingCheckpoint(t *testing.T) {
	t.Setenv("CPA_CURSOR_CONV_CACHE_DIR", t.TempDir())
	const account = "auth:race"
	const model = "claude-opus-5"
	opening := []ChatMessage{{Role: "user", Content: "inspect the repo"}}
	fp := conversationFingerprint(opening)
	scope := convScope(account, model)
	resolve := beginPendingAll(scope, []string{fp})
	t.Cleanup(func() { defaultConversationCache.Invalidate(scope, fp) })

	continuation := append(append([]ChatMessage(nil), opening...),
		ChatMessage{Role: "assistant", Content: "checking"},
		ChatMessage{Role: "tool", ToolCallID: "call-1", Content: "result"},
	)
	go func() {
		time.Sleep(20 * time.Millisecond)
		defaultConversationCache.Store(scope, fp, &convEntry{
			conversationID: "conv-race",
			state:          testConversationState(),
			model:          model,
		})
		resolve()
	}()

	entry, gotFingerprint, folded, prefix, ok := lookupPrefixResume(account, model, continuation)
	if !ok || entry.conversationID != "conv-race" {
		t.Fatal("a follow-up must wait for the announced checkpoint store")
	}
	if gotFingerprint != fp || folded == "" || len(prefix) != 1 {
		t.Fatalf("unexpected boundary result fp=%q folded=%q prefix=%d", gotFingerprint, folded, len(prefix))
	}
}

func TestEveryCompletedTurnWritesANewCheckpoint(t *testing.T) {
	t.Setenv("CPA_CURSOR_CONV_CACHE_DIR", t.TempDir())
	scope := convScope("auth:loop", "claude-opus-5")
	firstRequest := []ChatMessage{{Role: "user", Content: "turn one"}}
	first := &Session{
		ConversationID: "conv-loop",
		Model:          "claude-opus-5",
		convScope:      scope,
		transcript:     append([]ChatMessage(nil), firstRequest...),
		requestKey:     conversationFingerprint(firstRequest),
		checkpoint:     testConversationState(),
		ckptAfterEnd:   true,
		blobStore:      map[string][]byte{},
	}
	first.segText.WriteString("answer one")
	first.storeConversationSnapshot()

	firstTranscript := append(append([]ChatMessage(nil), firstRequest...),
		ChatMessage{Role: "assistant", Content: "answer one"})
	firstFP := conversationFingerprint(firstTranscript)
	if _, ok := defaultConversationCache.Lookup(scope, firstFP); !ok {
		t.Fatal("first turn checkpoint was not stored")
	}

	secondRequest := append(append([]ChatMessage(nil), firstTranscript...),
		ChatMessage{Role: "user", Content: "turn two"})
	second := &Session{
		ConversationID: "conv-loop",
		Model:          "claude-opus-5",
		convScope:      scope,
		transcript:     append([]ChatMessage(nil), secondRequest...),
		requestKey:     conversationFingerprint(secondRequest),
		checkpoint: &agentv1.ConversationStateStructure{
			Turns: [][]byte{[]byte("turn-1"), []byte("turn-2")},
		},
		ckptAfterEnd: true,
		blobStore:    map[string][]byte{},
	}
	second.segText.WriteString("answer two")
	second.storeConversationSnapshot()

	secondTranscript := append(append([]ChatMessage(nil), secondRequest...),
		ChatMessage{Role: "assistant", Content: "answer two"})
	secondFP := conversationFingerprint(secondTranscript)
	entry, ok := defaultConversationCache.Lookup(scope, secondFP)
	if !ok || len(entry.state.GetTurns()) != 2 {
		t.Fatal("the second turn did not update the conversation checkpoint")
	}
	defaultConversationCache.Invalidate(scope, first.requestKey)
	defaultConversationCache.Invalidate(scope, firstFP)
	defaultConversationCache.Invalidate(scope, second.requestKey)
	defaultConversationCache.Invalidate(scope, secondFP)
}

func TestBuildResumeRunRequestKeepsConversationAndCheckpoint(t *testing.T) {
	entry := &convEntry{
		conversationID: "conv-keep",
		state:          testConversationState(),
		blobs:          map[string][]byte{"blob": []byte("payload")},
		model:          "claude-opus-5",
	}
	message, blobs, id, err := buildResumeRunRequest(
		"claude-5-opus",
		entry,
		"next question",
		[]ToolDefinition{{Name: "lookup"}},
		ToolChoice{},
	)
	if err != nil {
		t.Fatal(err)
	}
	run := message.GetRunRequest()
	if id != "conv-keep" || run.GetConversationId() != "conv-keep" {
		t.Fatalf("resume changed conversation id: %q / %q", id, run.GetConversationId())
	}
	if len(run.GetConversationState().GetTurns()) != 1 {
		t.Fatal("resume dropped the server checkpoint turns")
	}
	if string(blobs["blob"]) != "payload" {
		t.Fatal("resume dropped checkpoint-referenced blobs")
	}
}
