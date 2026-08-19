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

func TestAddCursorKeyPostsOnlyTheNewEntry(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST so existing keys are never resent", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode post body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(0, "secret")
	client.baseURL = server.URL

	if err := client.AddCursorKey("crsr_new"); err != nil {
		t.Fatalf("AddCursorKey: %v", err)
	}
	if received["api-key"] != "crsr_new" {
		t.Fatalf("unexpected body: %+v", received)
	}
}
