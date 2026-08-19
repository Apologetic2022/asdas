package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestPutCursorKeysPersistsEntries(t *testing.T) {
	configPath := writeTestConfigFile(t)
	h := &Handler{cfg: &config.Config{}, configFilePath: configPath}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/v0/management/cursor-api-key",
		strings.NewReader(`[{"api-key":"  crsr_test  ","prefix":"cursor","priority":3}]`))

	h.PutCursorKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(h.cfg.CursorKey); got != 1 {
		t.Fatalf("cursor keys len = %d, want 1", got)
	}
	if got := h.cfg.CursorKey[0].APIKey; got != "crsr_test" {
		t.Fatalf("api-key = %q, want %q", got, "crsr_test")
	}

	data, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("read config: %v", errRead)
	}
	if !strings.Contains(string(data), "cursor-api-key:") {
		t.Fatalf("config file missing cursor-api-key block:\n%s", data)
	}
}

func TestGetCursorKeysReturnsEntries(t *testing.T) {
	h := &Handler{cfg: &config.Config{
		CursorKey: []config.CursorKey{{APIKey: "crsr_test", Prefix: "cursor"}},
	}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-api-key", nil)

	h.GetCursorKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		CursorKey []struct {
			APIKey string `json:"api-key"`
			Prefix string `json:"prefix"`
		} `json:"cursor-api-key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.CursorKey) != 1 || body.CursorKey[0].APIKey != "crsr_test" || body.CursorKey[0].Prefix != "cursor" {
		t.Fatalf("unexpected payload: %s", rec.Body.String())
	}
}

func TestPatchCursorKeyUpdatesMatchedEntry(t *testing.T) {
	h := &Handler{
		cfg: &config.Config{
			CursorKey: []config.CursorKey{
				{APIKey: "crsr_a"},
				{APIKey: "crsr_b"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/cursor-api-key",
		strings.NewReader(`{"match":"crsr_b","value":{"base-url":" https://proxy.example.com ","disable-cooling":true}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchCursorKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	entry := h.cfg.CursorKey[1]
	if entry.BaseURL != "https://proxy.example.com" {
		t.Fatalf("base-url = %q, want %q", entry.BaseURL, "https://proxy.example.com")
	}
	if !entry.DisableCooling {
		t.Fatal("disable-cooling = false, want true")
	}
}

func TestDeleteCursorKeyRequiresBaseURLWhenAPIKeyDuplicated(t *testing.T) {
	h := &Handler{
		cfg: &config.Config{
			CursorKey: []config.CursorKey{
				{APIKey: "shared-key", BaseURL: "https://a.example.com"},
				{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/cursor-api-key?api-key=shared-key", nil)

	h.DeleteCursorKey(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := len(h.cfg.CursorKey); got != 2 {
		t.Fatalf("cursor keys len = %d, want 2", got)
	}
}

func TestDeleteCursorKeyDeletesOnlyMatchingBaseURL(t *testing.T) {
	h := &Handler{
		cfg: &config.Config{
			CursorKey: []config.CursorKey{
				{APIKey: "shared-key", BaseURL: "https://a.example.com"},
				{APIKey: "shared-key", BaseURL: "https://b.example.com"},
			},
		},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodDelete, "/v0/management/cursor-api-key?api-key=shared-key&base-url=https://a.example.com", nil)

	h.DeleteCursorKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(h.cfg.CursorKey); got != 1 {
		t.Fatalf("cursor keys len = %d, want 1", got)
	}
	if got := h.cfg.CursorKey[0].BaseURL; got != "https://b.example.com" {
		t.Fatalf("remaining base-url = %q, want %q", got, "https://b.example.com")
	}
}
