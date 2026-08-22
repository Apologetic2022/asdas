package executor

import (
	"testing"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
)

func TestExtractToolChoice(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    cursorlib.ToolChoice
	}{
		{"absent", `{}`, cursorlib.ToolChoice{}},
		{"auto", `{"tool_choice":"auto"}`, cursorlib.ToolChoice{Mode: "auto"}},
		{"none", `{"tool_choice":"none"}`, cursorlib.ToolChoice{Mode: "none"}},
		{"required", `{"tool_choice":"required"}`, cursorlib.ToolChoice{Mode: "required"}},
		{"any", `{"tool_choice":"any"}`, cursorlib.ToolChoice{Mode: "required"}},
		{"function", `{"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`, cursorlib.ToolChoice{Mode: "function", FunctionName: "get_weather"}},
		{"claude-tool", `{"tool_choice":{"type":"tool","name":"get_weather"}}`, cursorlib.ToolChoice{Mode: "function", FunctionName: "get_weather"}},
		{"claude-any", `{"tool_choice":{"type":"any"}}`, cursorlib.ToolChoice{Mode: "required"}},
	}
	for _, tc := range cases {
		if got := extractToolChoice([]byte(tc.payload)); got != tc.want {
			t.Errorf("%s: got %#v want %#v", tc.name, got, tc.want)
		}
	}
}

func TestTrailingToolCallIDs(t *testing.T) {
	openai := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"ok"}
	]}`)
	if ids := trailingToolCallIDs(openai); len(ids) != 1 || ids[0] != "c1" {
		t.Fatalf("openai trailing ids = %#v", ids)
	}
	claude := []byte(`{"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"f","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"ok"}]},
		{"role":"system","content":"<total_tokens>100 tokens left</total_tokens>"}
	]}`)
	if ids := trailingToolCallIDs(claude); len(ids) != 1 || ids[0] != "c2" {
		t.Fatalf("claude trailing ids = %#v", ids)
	}
	plain := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	if ids := trailingToolCallIDs(plain); len(ids) != 0 {
		t.Fatalf("plain request should have no trailing ids, got %#v", ids)
	}
}

func TestRestoreClaudeSystemMessageRoles(t *testing.T) {
	original := []byte(`{"messages":[
		{"role":"user","content":"这个html给我删除"},
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"permission denied"}]},
		{"role":"system","content":"<total_tokens>15000000 tokens left</total_tokens>"}
	]}`)
	messages := []cursorlib.ChatMessage{
		{Role: "user", Content: "这个html给我删除"},
		{Role: "assistant", ToolCalls: []cursorlib.ToolCall{{ID: "c1", Name: "bash"}}},
		{Role: "tool", ToolCallID: "c1", Content: "permission denied"},
		{Role: "user", Content: "<system-reminder>\n<total_tokens>15000000 tokens left</total_tokens>\n</system-reminder>"},
	}

	restoreClaudeSystemMessageRoles(messages, original)

	if got := messages[3].Role; got != "system" {
		t.Fatalf("translated reminder role = %q, want system", got)
	}
	if got := messages[0].Role; got != "user" {
		t.Fatalf("real user role changed to %q", got)
	}
}

func TestOpenAIUsagePayloadTreatsCacheCountersAsPromptSubsets(t *testing.T) {
	tokens := cursorlib.TokenUsage{
		InputTokens:      132250,
		OutputTokens:     48,
		CacheReadTokens:  130000,
		CacheWriteTokens: 250,
		ReasoningTokens:  16,
	}

	payload := openAIUsagePayload(tokens)
	if got := payload["prompt_tokens"].(int64); got != 132250 {
		t.Fatalf("prompt tokens = %d, want the fresh read plus both cached parts", got)
	}
	if got := payload["total_tokens"].(int64); got != 132298 {
		t.Fatalf("total tokens = %d, want 132298", got)
	}
	details := payload["prompt_tokens_details"].(map[string]any)
	if got := details["cached_tokens"].(int64); got != 130000 {
		t.Fatalf("cached tokens = %d, want 130000", got)
	}
	if got := details["cache_creation_tokens"].(int64); got != 250 {
		t.Fatalf("cache creation tokens = %d, want 250", got)
	}

	detail := usageDetail(tokens)
	if detail.InputTokens != 2000 {
		t.Fatalf("detail input = %d, want only the fresh tokens", detail.InputTokens)
	}
	if detail.CacheReadTokens != 130000 || detail.CacheCreationTokens != 250 {
		t.Fatalf("detail cache split = %d/%d, want 130000/250", detail.CacheReadTokens, detail.CacheCreationTokens)
	}
	if detail.TotalTokens != 132298 {
		t.Fatalf("detail total = %d, want 132298", detail.TotalTokens)
	}
}
