package cliproxy

import (
	"path/filepath"
	"testing"
	"time"

	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
)

func catalogSnapshot(fetchedAt time.Time, ids ...string) *cursorlib.CatalogSnapshot {
	models := make([]cursorlib.CatalogModel, 0, len(ids))
	for _, id := range ids {
		models = append(models, cursorlib.CatalogModel{ID: id, DisplayName: id})
	}
	return &cursorlib.CatalogSnapshot{FetchedAt: fetchedAt, Models: models}
}

func TestCursorCatalogStoreServesOwnCatalogAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), cursorCatalogCacheFile)
	now := time.Now().UTC().Truncate(time.Second)

	newCursorCatalogStore(path).remember("cursor:apikey:aaa", catalogSnapshot(now, "grok-4.6", "claude-opus-5"))

	// A restart reads the catalog back from disk, so an upstream outage during
	// startup no longer collapses the model list to the builtins.
	restarted := newCursorCatalogStore(path)
	snapshot, source := restarted.lookup("cursor:apikey:aaa")
	if snapshot == nil {
		t.Fatal("expected the persisted catalog to be found")
	}
	if source != "own" {
		t.Fatalf("source = %q, want own", source)
	}
	if len(snapshot.Models) != 2 || snapshot.Models[0].ID != "grok-4.6" {
		t.Fatalf("models = %+v, want grok-4.6 and claude-opus-5", snapshot.Models)
	}
	if !snapshot.FetchedAt.Equal(now) {
		t.Fatalf("fetched at = %s, want %s", snapshot.FetchedAt, now)
	}
}

func TestCursorCatalogStoreFallsBackToNewestSibling(t *testing.T) {
	store := newCursorCatalogStore(filepath.Join(t.TempDir(), cursorCatalogCacheFile))
	older := time.Now().Add(-time.Hour)
	store.remember("cursor:apikey:old", catalogSnapshot(older, "grok-4.5"))
	store.remember("cursor:apikey:new", catalogSnapshot(time.Now(), "grok-4.6"))

	// A key that was just added has no catalog of its own; the sibling entry is
	// a far better answer than the three-model builtin list.
	snapshot, source := store.lookup("cursor:apikey:fresh")
	if snapshot == nil {
		t.Fatal("expected a sibling catalog to be found")
	}
	if source != "sibling" {
		t.Fatalf("source = %q, want sibling", source)
	}
	if len(snapshot.Models) != 1 || snapshot.Models[0].ID != "grok-4.6" {
		t.Fatalf("models = %+v, want the newest sibling catalog", snapshot.Models)
	}
}

func TestCursorCatalogStoreReportsNothingWhenEmpty(t *testing.T) {
	store := newCursorCatalogStore(filepath.Join(t.TempDir(), cursorCatalogCacheFile))
	if snapshot, source := store.lookup("cursor:apikey:aaa"); snapshot != nil || source != "" {
		t.Fatalf("lookup = %+v/%q, want no catalog", snapshot, source)
	}
}

func TestCursorCatalogStorePrunesOldestEntries(t *testing.T) {
	store := newCursorCatalogStore(filepath.Join(t.TempDir(), cursorCatalogCacheFile))
	base := time.Now().Add(-24 * time.Hour)
	for i := 0; i < cursorCatalogCacheKeep+3; i++ {
		store.remember(string(rune('a'+i)), catalogSnapshot(base.Add(time.Duration(i)*time.Minute), "grok-4.6"))
	}
	if got := len(store.byAuth); got != cursorCatalogCacheKeep {
		t.Fatalf("cached credentials = %d, want %d", got, cursorCatalogCacheKeep)
	}
	if _, ok := store.byAuth["a"]; ok {
		t.Fatal("expected the oldest credential to be pruned")
	}
}

func TestCursorCatalogRetriesRunOncePerAuth(t *testing.T) {
	retry := &cursorCatalogRetries{inFlight: map[string]struct{}{}, stop: make(chan struct{})}
	if !retry.begin("auth-a") {
		t.Fatal("first retry should start")
	}
	if retry.begin("auth-a") {
		t.Fatal("a second retry for the same auth should be refused")
	}
	if !retry.begin("auth-b") {
		t.Fatal("a retry for a different auth should start")
	}
	retry.done("auth-a")
	if !retry.begin("auth-a") {
		t.Fatal("a finished retry should be restartable")
	}
	retry.shutdown()
	if retry.begin("auth-c") {
		t.Fatal("no retry should start after shutdown")
	}
	select {
	case <-retry.stop:
	default:
		t.Fatal("shutdown should release the running retries")
	}
	retry.shutdown()
}
