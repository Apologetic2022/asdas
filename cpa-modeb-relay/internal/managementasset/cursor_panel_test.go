package managementasset

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInjectCursorPanelInsertsBeforeBodyClose(t *testing.T) {
	page := []byte("<html><body><div id=\"root\"></div></body></html>")

	out := string(InjectCursorPanel(page))

	if !strings.Contains(out, cursorPanelMarker) {
		t.Fatalf("panel not injected: %s", out)
	}
	if !strings.HasSuffix(out, "</body></html>") {
		t.Fatalf("panel injected outside the body: %s", out)
	}
	if strings.Index(out, "<div id=\"root\">") > strings.Index(out, cursorPanelMarker) {
		t.Fatal("panel injected before the page content")
	}
}

func TestInjectCursorPanelAppendsWhenBodyCloseMissing(t *testing.T) {
	out := string(InjectCursorPanel([]byte("<html>no body tag</html>")))

	if !strings.Contains(out, cursorPanelMarker) {
		t.Fatalf("panel not injected: %s", out)
	}
}

func TestInjectCursorPanelIsIdempotent(t *testing.T) {
	once := InjectCursorPanel([]byte("<html><body></body></html>"))

	twice := InjectCursorPanel(once)

	if !bytes.Equal(once, twice) {
		t.Fatal("second injection changed the page")
	}
}

func TestControlPanelContentRefreshesAfterFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), ManagementFileName)
	if err := os.WriteFile(path, []byte("<html><body>v1</body></html>"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	first, _, err := ControlPanelContent(path)
	if err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if !strings.Contains(string(first), "v1") || !strings.Contains(string(first), cursorPanelMarker) {
		t.Fatalf("unexpected first read: %s", first)
	}

	// Same size, so the cache has to notice the modification time instead.
	if err = os.WriteFile(path, []byte("<html><body>v2</body></html>"), 0o600); err != nil {
		t.Fatalf("rewrite asset: %v", err)
	}
	if err = os.Chtimes(path, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatalf("touch asset: %v", err)
	}

	second, _, err := ControlPanelContent(path)
	if err != nil {
		t.Fatalf("reread asset: %v", err)
	}
	if !strings.Contains(string(second), "v2") {
		t.Fatalf("cached stale asset: %s", second)
	}
}
