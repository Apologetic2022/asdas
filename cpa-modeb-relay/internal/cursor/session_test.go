package cursor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestExtractToolResultsTrailingOnly(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "old", Name: "x", Arguments: map[string]any{}}}},
		{Role: "tool", ToolCallID: "old", Content: "stale"},
		{Role: "user", Content: "again"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "NY"}}}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"ok":true}`},
		{Role: "system", Content: "<total_tokens>15000000 tokens left</total_tokens>"},
	}
	got := extractToolResults(messages)
	if len(got) != 1 || got[0].ToolCallID != "c1" || got[0].Content != `{"ok":true}` {
		t.Fatalf("expected trailing c1, got %#v", got)
	}
}

func TestBuildRunRequestIncludesMcpTools(t *testing.T) {
	msg, _, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "weather?"},
	}, []ToolDefinition{{
		Name:        "get_weather",
		Description: "weather",
		Parameters:  map[string]any{"type": "object"},
	}}, ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	if run == nil || run.GetMcpTools() == nil || len(run.GetMcpTools().GetMcpTools()) != 1 {
		t.Fatalf("expected mcp tools on run request, got %#v", run.GetMcpTools())
	}
	tool := run.GetMcpTools().GetMcpTools()[0]
	if tool.GetProviderIdentifier() != MCPProviderIdentifier || tool.GetName() != "get_weather" {
		t.Fatalf("unexpected tool: %#v", tool)
	}
}

func TestBuildRunRequestNativeToolJSON(t *testing.T) {
	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "thinking", ToolCalls: []ToolCall{
			{ID: "c1", Name: "get_weather", Arguments: map[string]any{"city": "NY"}},
		}},
		{Role: "tool", ToolCallID: "c1", Name: "get_weather", Content: `{"ok":true}`},
		{Role: "user", Content: "again"},
	}, nil, ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	if msg.GetRunRequest() == nil {
		t.Fatal("missing run request")
	}
	var sawToolCall, sawToolResult bool
	for _, data := range blobs {
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			continue
		}
		role, _ := payload["role"].(string)
		content, _ := payload["content"].([]any)
		switch role {
		case "assistant":
			for _, part := range content {
				m, _ := part.(map[string]any)
				if m["type"] == "tool-call" && m["toolCallId"] == "c1" {
					sawToolCall = true
				}
			}
			if _, ok := payload["tool_calls"]; ok {
				t.Fatalf("expected Cursor-native tool-call parts, not OpenAI tool_calls: %#v", payload)
			}
		case "tool":
			if payload["id"] != "c1" {
				continue
			}
			for _, part := range content {
				m, _ := part.(map[string]any)
				if m["type"] == "tool-result" && m["toolCallId"] == "c1" {
					sawToolResult = true
				}
			}
			if _, ok := payload["tool_call_id"]; ok {
				t.Fatalf("expected Cursor-native tool id/content, not tool_call_id: %#v", payload)
			}
		}
	}
	if !sawToolCall || !sawToolResult {
		t.Fatalf("native tool history missing call=%v result=%v", sawToolCall, sawToolResult)
	}
}

func TestBuildRunRequestResumeCarriesTrailingToolResults(t *testing.T) {
	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "weather in NY?"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "c9", Name: "get_weather", Arguments: map[string]any{"city": "NY"}},
		}},
		{Role: "tool", ToolCallID: "c9", Name: "get_weather", Content: `{"temp":21}`},
	}, nil, ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	action := msg.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if action != resumeContinuationPrompt {
		t.Fatalf("expected continuation prompt action, got %q", action)
	}
	sawResult := false
	for _, data := range blobs {
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		if payload["role"] == "tool" && payload["id"] == "c9" {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatal("trailing tool result missing from rebuilt history")
	}
}

func TestBuildRunRequestToolChoiceDirective(t *testing.T) {
	_, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "what's the weather?"},
	}, []ToolDefinition{{Name: "get_weather"}}, ToolChoice{Mode: "required"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, data := range blobs {
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		if payload["role"] == "system" {
			if content, _ := payload["content"].(string); strings.Contains(content, "Tool call required") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("tool_choice required directive missing from run request")
	}
}

func TestApplyToolChoice(t *testing.T) {
	tools := []ToolDefinition{{Name: "a"}, {Name: "b"}}
	if got := ApplyToolChoice(tools, ToolChoice{Mode: "none"}); got != nil {
		t.Fatalf("none should drop tools, got %#v", got)
	}
	if got := ApplyToolChoice(tools, ToolChoice{Mode: "function", FunctionName: "b"}); len(got) != 1 || got[0].Name != "b" {
		t.Fatalf("function should narrow tools, got %#v", got)
	}
	if got := ApplyToolChoice(tools, ToolChoice{Mode: "required"}); len(got) != 2 {
		t.Fatalf("required should keep tools, got %#v", got)
	}
}

func TestCookieJarRemembersSetCookie(t *testing.T) {
	jar := &CookieJar{byHost: map[string]map[string]string{}}
	hdr := make(http.Header)
	hdr.Add("Set-Cookie", "CursorCookie=server-issued; Path=/; HttpOnly")
	jar.RememberResponse("https://api2.cursor.sh", hdr)
	got := jar.Header("https://api2.cursor.sh")
	if !strings.Contains(got, "CursorCookie=server-issued") {
		t.Fatalf("expected server cookie, got %q", got)
	}
}

func TestProtobufValueRoundTrip(t *testing.T) {
	in := map[string]any{"city": "北京", "n": float64(1), "ok": true}
	v, err := toProtobufValue(in)
	if err != nil {
		t.Fatal(err)
	}
	out := fromProtobufValue(v)
	raw, _ := json.Marshal(out)
	if string(raw) == "" {
		t.Fatal("empty roundtrip")
	}
}

func TestFromProtobufValueKeepsStringsVerbatim(t *testing.T) {
	// Strings that look like JSON must NOT be re-parsed: "true" is a shell
	// command, "{\"a\":1}" is a file body, "123" is a literal string arg.
	for _, s := range []string{"true", "false", "null", "123", `{"a":1}`, `[1,2]`, "ls -la"} {
		v, err := toProtobufValue(s)
		if err != nil {
			t.Fatal(err)
		}
		out := fromProtobufValue(v)
		got, ok := out.(string)
		if !ok || got != s {
			t.Fatalf("string %q corrupted to %#v", s, out)
		}
	}
}

func TestBuildRunRequestMcpOnlyDirective(t *testing.T) {
	msg, blobs, _, err := buildRunRequest("default", []ChatMessage{
		{Role: "user", Content: "list the workspace"},
	}, []ToolDefinition{{Name: "Task"}, {Name: "Bash"}}, ToolChoice{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, data := range blobs {
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		if payload["role"] == "system" {
			if content, _ := payload["content"].(string); strings.Contains(content, "ONLY tools available") &&
				strings.Contains(content, "mcp_Task") && strings.Contains(content, "mcp_Bash") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("mcp-only tool directive missing from run request")
	}
	action := msg.GetRunRequest().GetAction().GetUserMessageAction().GetUserMessage().GetText()
	if !strings.Contains(action, "Tool constraint") {
		t.Fatalf("action text missing tool constraint: %q", action)
	}
}

func TestCanonicalPendingIDFuzzyMatch(t *testing.T) {
	m := NewSessionManager()
	sess := &Session{ID: "s1", manager: m}
	orig := "fc_ownxAbq-3LYxF7-a2d74f5f8eac5702_0"
	m.BindPending(orig, sess)

	// Exact.
	if got := m.LookupPending(orig); got != sess {
		t.Fatal("exact lookup failed")
	}
	// Client-side rewrite (ZCode style: prefix + original).
	rewritten := "call-80338b6a-2b74-4b47-bd1c-3d18fae36cdd-13_" + orig
	if got := m.LookupPending(rewritten); got != sess {
		t.Fatal("rewritten id lookup failed")
	}
	// Unrelated id must not match.
	if got := m.LookupPending("call-ffff-1_fc_other_9"); got != nil {
		t.Fatal("unrelated id must not match")
	}

	owner, normalized, err := m.ResolveForToolResults([]ToolResult{{ToolCallID: rewritten, Content: "ok"}})
	if err != nil || owner != sess {
		t.Fatalf("resolve failed: %v", err)
	}
	if normalized[0].ToolCallID != orig {
		t.Fatalf("expected normalization to %q, got %q", orig, normalized[0].ToolCallID)
	}
}

func TestUnknownProtoFieldNumbers(t *testing.T) {
	// field 99, varint 1: tag = 99<<3 | 0 = 0x318 → bytes 0x98 0x06, value 0x01
	raw := []byte{0x98, 0x06, 0x01}
	fields := unknownProtoFieldNumbers(raw)
	if len(fields) != 1 || fields[0] != 99 {
		t.Fatalf("expected [99], got %v", fields)
	}
}

func TestWireToolNamePrefixesNativeCollisions(t *testing.T) {
	cases := map[string]string{
		"Task":        "mcp_Task",
		"task":        "mcp_task",
		"Bash":        "mcp_Bash",
		"Glob":        "mcp_Glob",
		"Grep":        "mcp_Grep",
		"Read":        "mcp_Read",
		"Write":       "mcp_Write",
		"TodoWrite":   "mcp_TodoWrite",
		"WebFetch":    "mcp_WebFetch",
		"get_weather": "get_weather",
		"MyCustom":    "MyCustom",
	}
	for in, want := range cases {
		if got := wireToolName(in); got != want {
			t.Fatalf("wireToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIndexToolsMapsWireAndClientNames(t *testing.T) {
	tools := []ToolDefinition{{Name: "Task"}, {Name: "get_weather"}}
	idx := indexTools(tools)
	if idx["Task"] == nil || idx["mcp_Task"] == nil || idx["Task"] != idx["mcp_Task"] {
		t.Fatalf("wire alias not indexed: %#v", idx)
	}
	if idx["get_weather"] == nil {
		t.Fatal("plain tool missing from index")
	}
	if idx["mcp_Task"].Name != "Task" {
		t.Fatalf("client name must be preserved, got %q", idx["mcp_Task"].Name)
	}
}
