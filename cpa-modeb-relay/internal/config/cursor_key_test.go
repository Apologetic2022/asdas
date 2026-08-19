package config

import "testing"

func TestParseConfigBytesSanitizesCursorKeys(t *testing.T) {
	raw := []byte(`
cursor-api-key:
  - api-key: "  crsr_demo  "
    priority: 2
    prefix: "cursor"
    base-url: "https://api2.cursor.sh"
    excluded-models:
      - "  gpt-5.1  "
  - api-key: "crsr_demo"
    base-url: "https://api2.cursor.sh"
  - api-key: "   "
`)

	cfg, err := ParseConfigBytes(raw)
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(cfg.CursorKey) != 1 {
		t.Fatalf("cursor keys = %d, want 1 (%#v)", len(cfg.CursorKey), cfg.CursorKey)
	}
	entry := cfg.CursorKey[0]
	if entry.APIKey != "crsr_demo" {
		t.Fatalf("api-key = %q, want %q", entry.APIKey, "crsr_demo")
	}
	if entry.Priority != 2 {
		t.Fatalf("priority = %d, want 2", entry.Priority)
	}
	if entry.Prefix != "cursor" {
		t.Fatalf("prefix = %q, want %q", entry.Prefix, "cursor")
	}
	if len(entry.ExcludedModels) != 1 || entry.ExcludedModels[0] != "gpt-5.1" {
		t.Fatalf("excluded-models = %#v, want [gpt-5.1]", entry.ExcludedModels)
	}
}

func TestSanitizeCursorKeysKeepsDistinctBaseURLs(t *testing.T) {
	cfg := &Config{
		CursorKey: []CursorKey{
			{APIKey: "crsr_demo"},
			{APIKey: "crsr_demo", BaseURL: "https://mirror.example.com"},
		},
	}

	cfg.SanitizeCursorKeys()

	if len(cfg.CursorKey) != 2 {
		t.Fatalf("cursor keys = %d, want 2 (%#v)", len(cfg.CursorKey), cfg.CursorKey)
	}
}
