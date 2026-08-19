package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestDoCursorAPIKeyWritesConfigBlock(t *testing.T) {
	path := writeTempConfig(t, "port: 8317\n")
	cfg := &config.Config{}

	DoCursorAPIKey(cfg, path, "  crsr_example  ")

	if len(cfg.CursorKey) != 1 || cfg.CursorKey[0].APIKey != "crsr_example" {
		t.Fatalf("config not updated in memory: %+v", cfg.CursorKey)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(data), "cursor-api-key:") || !strings.Contains(string(data), "crsr_example") {
		t.Fatalf("cursor-api-key block missing from config:\n%s", data)
	}
}

func TestDoCursorAPIKeyIsIdempotent(t *testing.T) {
	path := writeTempConfig(t, "port: 8317\n")
	cfg := &config.Config{}

	DoCursorAPIKey(cfg, path, "crsr_example")
	DoCursorAPIKey(cfg, path, "crsr_example")

	if len(cfg.CursorKey) != 1 {
		t.Fatalf("duplicate entries stored: %+v", cfg.CursorKey)
	}
}

func TestDoCursorAPIKeyRejectsEmptyKey(t *testing.T) {
	path := writeTempConfig(t, "port: 8317\n")
	cfg := &config.Config{}

	DoCursorAPIKey(cfg, path, "   ")

	if len(cfg.CursorKey) != 0 {
		t.Fatalf("empty key stored: %+v", cfg.CursorKey)
	}
}
