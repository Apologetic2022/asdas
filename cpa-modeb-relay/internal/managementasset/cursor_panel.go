package managementasset

import (
	"bytes"
	_ "embed"
	"os"
	"sync"
	"time"
)

// The upstream control panel has no Cursor section, so the Cursor credential list
// would only be reachable over curl. This snippet adds one to the page the relay
// serves, talking to /v0/management/cursor-api-key like the rest of the panel does.
//
//go:embed cursor_panel.html
var cursorPanelSnippet []byte

// cursorPanelMarker identifies an already-injected page, so re-serving a cached
// copy or a hand-patched management.html cannot stack two panels.
const cursorPanelMarker = `id="cpa-cursor-api-key-manager-script"`

var bodyClose = []byte("</body>")

// InjectCursorPanel returns page with the Cursor API key manager appended to its body.
func InjectCursorPanel(page []byte) []byte {
	if len(page) == 0 || bytes.Contains(page, []byte(cursorPanelMarker)) {
		return page
	}
	out := make([]byte, 0, len(page)+len(cursorPanelSnippet))
	if idx := bytes.LastIndex(bytes.ToLower(page), bodyClose); idx >= 0 {
		out = append(out, page[:idx]...)
		out = append(out, cursorPanelSnippet...)
		out = append(out, page[idx:]...)
		return out
	}
	out = append(out, page...)
	out = append(out, cursorPanelSnippet...)
	return out
}

// panelCache holds the injected page so the 2.5 MB asset is only rewritten when
// the file on disk changes.
var panelCache struct {
	sync.Mutex
	path    string
	modTime time.Time
	size    int64
	content []byte
}

// ControlPanelContent reads management.html from path and returns it with the
// Cursor API key manager injected, along with the modification time callers
// should serve it under.
func ControlPanelContent(path string) ([]byte, time.Time, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, err
	}

	panelCache.Lock()
	if panelCache.content != nil &&
		panelCache.path == path &&
		panelCache.size == info.Size() &&
		panelCache.modTime.Equal(info.ModTime()) {
		content := panelCache.content
		panelCache.Unlock()
		return content, info.ModTime(), nil
	}
	panelCache.Unlock()

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, err
	}
	content := InjectCursorPanel(raw)

	panelCache.Lock()
	panelCache.path = path
	panelCache.size = info.Size()
	panelCache.modTime = info.ModTime()
	panelCache.content = content
	panelCache.Unlock()

	return content, info.ModTime(), nil
}
