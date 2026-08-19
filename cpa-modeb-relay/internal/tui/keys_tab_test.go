package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newKeysTabForTest(client *Client) keysTabModel {
	m := newKeysTabModel(client)
	m.SetSize(100, 40)
	return m
}

func pressKeys(t *testing.T, m keysTabModel, keys ...string) keysTabModel {
	t.Helper()
	for _, key := range keys {
		var msg tea.KeyMsg
		if len(key) == 1 {
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
		} else {
			t.Fatalf("unsupported key %q", key)
		}
		m, _ = m.Update(msg)
	}
	return m
}

func TestKeysTabRendersCursorSection(t *testing.T) {
	m := newKeysTabForTest(nil)
	m.cursorKeys = []map[string]any{
		{"api-key": "crsr_abcdefghijklmnop", "prefix": "cursor"},
	}
	m.viewport.SetContent(m.renderContent())

	content := m.renderContent()
	if !strings.Contains(content, "Cursor API Keys (1)") {
		t.Fatalf("cursor section missing from keys tab:\n%s", content)
	}
	if strings.Contains(content, "crsr_abcdefghijklmnop") {
		t.Fatalf("cursor key rendered unmasked:\n%s", content)
	}
}

func TestKeysTabAddTargetsFocusedSection(t *testing.T) {
	m := newKeysTabForTest(nil)

	m = pressKeys(t, m, "a")
	if !m.adding || m.editSection != keysSectionAccess {
		t.Fatalf("add targeted section %d, want access section", m.editSection)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = pressKeys(t, m, "s", "a")
	if !m.adding || m.editSection != keysSectionCursor {
		t.Fatalf("add targeted section %d, want cursor section", m.editSection)
	}
}

func TestKeysTabNavigatesCursorSection(t *testing.T) {
	m := newKeysTabForTest(nil)
	m.cursorKeys = []map[string]any{
		{"api-key": "crsr_one"},
		{"api-key": "crsr_two"},
	}

	m = pressKeys(t, m, "s", "j")
	if m.cursorKeyIdx != 1 {
		t.Fatalf("cursor index = %d, want 1", m.cursorKeyIdx)
	}
	if m.cursor != 0 {
		t.Fatalf("access key index changed to %d while cursor section focused", m.cursor)
	}
	if got := m.focusedKey(); got != "crsr_two" {
		t.Fatalf("focused key = %q, want crsr_two", got)
	}
}

func TestAddCursorKeyAppendsToExistingEntries(t *testing.T) {
	var received []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cursor-api-key": []map[string]any{
					{"api-key": "crsr_existing", "auth-index": "cursor-1"},
				},
			})
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
				t.Errorf("decode put body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	client := NewClient(0, "secret")
	client.baseURL = server.URL

	if err := client.AddCursorKey("crsr_new"); err != nil {
		t.Fatalf("AddCursorKey: %v", err)
	}
	if len(received) != 2 {
		t.Fatalf("sent %d entries, want 2: %+v", len(received), received)
	}
	if _, ok := received[0]["auth-index"]; ok {
		t.Fatalf("auth-index was echoed back to the server: %+v", received[0])
	}
	if received[0]["api-key"] != "crsr_existing" || received[1]["api-key"] != "crsr_new" {
		t.Fatalf("unexpected entries: %+v", received)
	}
}
