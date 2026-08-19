package cursor

import (
	"testing"
	"time"
)

func resetCatalogCache(t *testing.T) {
	t.Helper()
	globalCatalogCache.mu.Lock()
	globalCatalogCache.byModel = map[string]CatalogModel{}
	globalCatalogCache.usable = nil
	globalCatalogCache.fetched = time.Time{}
	globalCatalogCache.mu.Unlock()
}

func TestSnapshotCatalogCarriesUsableWireIDs(t *testing.T) {
	resetCatalogCache(t)
	models := []CatalogModel{{ID: "grok-4.6", WireID: "cursor-grok-4.6-high-fast"}}
	RememberCatalog(models)
	rememberUsableModels([]UsableModel{{ID: "cursor-grok-4.6-high-fast"}})

	snapshot := SnapshotCatalog(models)
	if snapshot == nil {
		t.Fatal("expected a snapshot")
	}
	if len(snapshot.Models) != 1 || snapshot.Models[0].WireID != "cursor-grok-4.6-high-fast" {
		t.Fatalf("models = %+v, want the wire id preserved", snapshot.Models)
	}
	if len(snapshot.Usable) != 1 || snapshot.Usable[0].ID != "cursor-grok-4.6-high-fast" {
		t.Fatalf("usable = %+v, want the usable model preserved", snapshot.Usable)
	}
	if snapshot.FetchedAt.IsZero() {
		t.Fatal("expected a fetch timestamp")
	}
}

func TestSnapshotCatalogIgnoresEmptyCatalog(t *testing.T) {
	resetCatalogCache(t)
	if snapshot := SnapshotCatalog(nil); snapshot != nil {
		t.Fatalf("snapshot = %+v, want nil", snapshot)
	}
}

func TestRestoreCatalogResolvesWireIDsAfterFailedFetch(t *testing.T) {
	resetCatalogCache(t)
	// Nothing has been fetched in this process, which is the state after a
	// restart while AvailableModels is unreachable.
	if sel := ResolveRequestedModel("grok-4.6"); sel.ModelID != "grok-4.6" || sel.VariantStringRepr {
		t.Fatalf("selection = %+v, want the bare public id", sel)
	}

	RestoreCatalog(&CatalogSnapshot{
		FetchedAt: time.Now(),
		Models:    []CatalogModel{{ID: "grok-4.6", WireID: "cursor-grok-4.6-high-fast"}},
		Usable:    []UsableModel{{ID: "cursor-grok-4.6-high-fast"}},
	})

	sel := ResolveRequestedModel("grok-4.6")
	if sel.ModelID != "cursor-grok-4.6-high-fast" || !sel.VariantStringRepr {
		t.Fatalf("selection = %+v, want the cached wire id", sel)
	}
}

func TestRestoreCatalogKeepsLiveEntries(t *testing.T) {
	resetCatalogCache(t)
	RememberCatalog([]CatalogModel{{ID: "grok-4.6", WireID: "cursor-grok-4.6-high"}})
	rememberUsableModels([]UsableModel{{ID: "cursor-grok-4.6-high"}})

	RestoreCatalog(&CatalogSnapshot{
		FetchedAt: time.Now().Add(-time.Hour),
		Models:    []CatalogModel{{ID: "grok-4.6", WireID: "stale-wire-id"}, {ID: "claude-opus-5"}},
		Usable:    []UsableModel{{ID: "stale-wire-id"}},
	})

	entry, ok := catalogEntry("grok-4.6")
	if !ok || entry.WireID != "cursor-grok-4.6-high" {
		t.Fatalf("entry = %+v, want the live wire id to win", entry)
	}
	if _, ok = catalogEntry("claude-opus-5"); !ok {
		t.Fatal("expected the cached-only model to be merged in")
	}
	usable := cachedUsableModels()
	if len(usable) != 1 || usable[0].ID != "cursor-grok-4.6-high" {
		t.Fatalf("usable = %+v, want the live list kept", usable)
	}
}

func TestRestoreCatalogIgnoresEmptySnapshot(t *testing.T) {
	resetCatalogCache(t)
	RestoreCatalog(nil)
	RestoreCatalog(&CatalogSnapshot{})
	if _, ok := catalogEntry("grok-4.6"); ok {
		t.Fatal("expected the cache to stay empty")
	}
}
