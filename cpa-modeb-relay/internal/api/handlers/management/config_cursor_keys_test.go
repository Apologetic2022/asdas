package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

func TestGetCursorKeysReportsPosition(t *testing.T) {
	h := &Handler{cfg: &config.Config{
		CursorKey: []config.CursorKey{{APIKey: "crsr_a"}, {APIKey: "crsr_b", Disabled: true}},
	}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-api-key", nil)

	h.GetCursorKeys(c)

	var body struct {
		CursorKey []struct {
			Index    int  `json:"index"`
			Disabled bool `json:"disabled"`
		} `json:"cursor-api-key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.CursorKey) != 2 {
		t.Fatalf("cursor keys len = %d, want 2: %s", len(body.CursorKey), rec.Body.String())
	}
	if body.CursorKey[0].Index != 0 || body.CursorKey[1].Index != 1 {
		t.Fatalf("indexes = %d,%d, want 0,1", body.CursorKey[0].Index, body.CursorKey[1].Index)
	}
	if body.CursorKey[0].Disabled || !body.CursorKey[1].Disabled {
		t.Fatalf("disabled flags = %v,%v, want false,true", body.CursorKey[0].Disabled, body.CursorKey[1].Disabled)
	}
}

func TestGetCursorKeysReportsRequestCounters(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	authID, _ := synthesizer.NewStableIDGenerator().Next("cursor:apikey", "crsr_a", "")
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:         authID,
		Provider:   "cursor",
		Attributes: map[string]string{"api_key": "crsr_a"},
	}); err != nil {
		t.Fatalf("register cursor auth: %v", err)
	}
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: authID, Provider: "cursor", Success: true})
	manager.MarkResult(context.Background(), coreauth.Result{AuthID: authID, Provider: "cursor", Success: false})

	h := NewHandlerWithoutConfigFilePath(&config.Config{
		CursorKey: []config.CursorKey{{APIKey: "crsr_a"}, {APIKey: "crsr_unused"}},
	}, manager)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v0/management/cursor-api-key", nil)

	h.GetCursorKeys(c)

	var body struct {
		CursorKey []struct {
			APIKey         string                         `json:"api-key"`
			Success        int64                          `json:"success"`
			Failed         int64                          `json:"failed"`
			RecentRequests []coreauth.RecentRequestBucket `json:"recent_requests"`
		} `json:"cursor-api-key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.CursorKey) != 2 {
		t.Fatalf("cursor keys len = %d, want 2: %s", len(body.CursorKey), rec.Body.String())
	}
	used := body.CursorKey[0]
	if used.Success != 1 || used.Failed != 1 {
		t.Fatalf("counters = %d/%d, want 1/1: %s", used.Success, used.Failed, rec.Body.String())
	}
	bucketSuccess, bucketFailed := sumRecentRequestBuckets(used.RecentRequests)
	if bucketSuccess != 1 || bucketFailed != 1 {
		t.Fatalf("recent buckets = %d/%d, want 1/1: %s", bucketSuccess, bucketFailed, rec.Body.String())
	}
	unused := body.CursorKey[1]
	if unused.Success != 0 || unused.Failed != 0 || len(unused.RecentRequests) != 0 {
		t.Fatalf("key without a live auth reported usage: %s", rec.Body.String())
	}
}

func TestPostCursorKeysAppendsWithoutResendingList(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{CursorKey: []config.CursorKey{{APIKey: "crsr_existing"}}},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/cursor-api-key",
		strings.NewReader(`{"api-key":"  crsr_new  ","prefix":"cursor"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PostCursorKeys(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := len(h.cfg.CursorKey); got != 2 {
		t.Fatalf("cursor keys len = %d, want 2", got)
	}
	if got := h.cfg.CursorKey[0].APIKey; got != "crsr_existing" {
		t.Fatalf("existing key = %q, want %q", got, "crsr_existing")
	}
	if got := h.cfg.CursorKey[1].APIKey; got != "crsr_new" {
		t.Fatalf("added key = %q, want %q", got, "crsr_new")
	}
}

func TestPostCursorKeysRejectsMissingKey(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t)}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v0/management/cursor-api-key",
		strings.NewReader(`{"prefix":"cursor"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PostCursorKeys(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if got := len(h.cfg.CursorKey); got != 0 {
		t.Fatalf("cursor keys len = %d, want 0", got)
	}
}

func TestPatchCursorKeyTogglesDisabledKeepingKey(t *testing.T) {
	h := &Handler{
		cfg:            &config.Config{CursorKey: []config.CursorKey{{APIKey: "crsr_a"}}},
		configFilePath: writeTestConfigFile(t),
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/cursor-api-key",
		strings.NewReader(`{"index":0,"value":{"disabled":true}}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.PatchCursorKey(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	entry := h.cfg.CursorKey[0]
	if !entry.Disabled {
		t.Fatal("disabled = false, want true")
	}
	if entry.APIKey != "crsr_a" {
		t.Fatalf("api-key = %q, want it left untouched", entry.APIKey)
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
