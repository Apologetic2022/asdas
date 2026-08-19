package cursor

import (
	"fmt"
	"testing"
)

func TestResolveRequestedModelUsesCatalogParams(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID:         "claude-sonnet-4-6",
		Parameters: []ModelParameter{{ID: "thinking", Value: "true"}},
	}})
	// The Anthropic-style client alias must resolve onto the catalog id.
	sel := ResolveRequestedModel("claude-4.6-sonnet")
	if sel.PublicID != "claude-sonnet-4-6" {
		t.Fatalf("public id = %q", sel.PublicID)
	}
	if len(sel.Parameters) != 1 || sel.Parameters[0].ID != "thinking" || sel.Parameters[0].Value != "true" {
		t.Fatalf("expected catalog thinking=true, got %#v", sel.Parameters)
	}
}

func TestResolveRequestedModelUsesWireID(t *testing.T) {
	RememberCatalog([]CatalogModel{{
		ID:         "grok-4.5",
		WireID:     "cursor-grok-4.5-high-fast",
		Parameters: []ModelParameter{{ID: "effort", Value: "high"}, {ID: "fast", Value: "true"}},
	}})
	sel := ResolveRequestedModel("grok-4.5")
	if sel.ModelID != "cursor-grok-4.5-high-fast" || !sel.VariantStringRepr {
		t.Fatalf("expected wire variant id, got %#v", sel)
	}
	if len(sel.Parameters) != 0 {
		t.Fatalf("variant string should not also send parameters: %#v", sel.Parameters)
	}
}

func TestResolveRequestedModelDefaultSelector(t *testing.T) {
	sel := ResolveRequestedModel("default")
	if sel.ModelID != "default" || len(sel.Parameters) != 0 {
		t.Fatalf("default selector should have no parameters, got %#v", sel)
	}
}

func TestResolveRequestedModelSuffix(t *testing.T) {
	sel := ResolveRequestedModel("claude-4.6-sonnet-thinking-xhigh")
	if sel.ModelID != "claude-sonnet-4-6" {
		t.Fatalf("base model = %q", sel.ModelID)
	}
	got := map[string]string{}
	for _, p := range sel.Parameters {
		got[p.ID] = p.Value
	}
	if got["thinking"] != "true" || got["reasoning"] != "xhigh" || got["effort"] != "xhigh" {
		t.Fatalf("unexpected parameters: %#v", sel.Parameters)
	}
}

func TestBuildRunRequestSendsParameters(t *testing.T) {
	msg, _, _, err := buildRunRequest("gpt-5.4-thinking", []ChatMessage{
		{Role: "user", Content: "hi"},
	}, nil, SessionOptions{ToolChoice: ToolChoice{}})
	if err != nil {
		t.Fatal(err)
	}
	run := msg.GetRunRequest()
	req := run.GetRequestedModel()
	if req.GetModelId() != "gpt-5.4" {
		t.Fatalf("requested model id = %q", req.GetModelId())
	}
	if len(req.GetParameters()) == 0 || req.GetParameters()[0].GetId() != "thinking" {
		t.Fatalf("expected thinking parameter, got %#v", req.GetParameters())
	}
}

func TestCanonicalizeModelID(t *testing.T) {
	cases := map[string]string{
		"claude-4.6-sonnet":          "claude-sonnet-4-6",
		"claude-4-6-sonnet":          "claude-sonnet-4-6",
		"claude-4-sonnet":            "claude-sonnet-4",
		"claude-4.5-haiku":           "claude-haiku-4-5",
		"claude-5-opus":              "claude-opus-5",
		"claude-4.5-sonnet-thinking": "claude-sonnet-4-5-thinking",
		"claude-sonnet-4-6":          "claude-sonnet-4-6",
		"grok-4.6":                   "grok-4.6",
		"gpt-5.2":                    "gpt-5.2",
		"claude-fable-5":             "claude-fable-5",
	}
	for in, want := range cases {
		if got := CanonicalizeModelID(in); got != want {
			t.Errorf("CanonicalizeModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientAliasForModelID(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-6":          "claude-4.6-sonnet",
		"claude-sonnet-4":            "claude-4-sonnet",
		"claude-haiku-4-5":           "claude-4.5-haiku",
		"claude-opus-5":              "claude-5-opus",
		"claude-sonnet-4-5-thinking": "claude-4.5-sonnet-thinking",
		"claude-fable-5":             "",
		"grok-4.6":                   "",
		"default":                    "",
	}
	for in, want := range cases {
		if got := ClientAliasForModelID(in); got != want {
			t.Errorf("ClientAliasForModelID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolveRequestedModelCanonicalizesClaudeAlias(t *testing.T) {
	sel := ResolveRequestedModel("claude-4.6-sonnet")
	if sel.PublicID != "claude-sonnet-4-6" {
		t.Fatalf("PublicID = %q, want claude-sonnet-4-6", sel.PublicID)
	}
	selThinking := ResolveRequestedModel("claude-4.5-sonnet-thinking")
	if selThinking.PublicID != "claude-sonnet-4-5" {
		t.Fatalf("thinking alias PublicID = %q, want claude-sonnet-4-5", selThinking.PublicID)
	}
}

func TestAutoSwitchModelFromError(t *testing.T) {
	err := fmt.Errorf(`cursor connect end-stream error: {"code":"resource_exhausted","message":"Error","details":[{"debug":{"error":"ERROR_RATE_LIMITED_CHANGEABLE","details":{"additionalInfo":{"autoSwitchToModel":"grok-4.6"}}}}]}`)
	if got := AutoSwitchModelFromError(err); got != "grok-4.6" {
		t.Fatalf("AutoSwitchModelFromError = %q, want grok-4.6", got)
	}
	if got := AutoSwitchModelFromError(fmt.Errorf("dial tcp: i/o timeout")); got != "" {
		t.Fatalf("unrelated error should yield empty, got %q", got)
	}
	if got := AutoSwitchModelFromError(nil); got != "" {
		t.Fatalf("nil error should yield empty, got %q", got)
	}
}
