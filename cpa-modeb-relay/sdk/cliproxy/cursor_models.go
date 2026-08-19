package cliproxy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	cursorlib "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// cursorCatalogCacheFile holds the last AvailableModels response of every
// Cursor credential, relative to the auth directory.
const cursorCatalogCacheFile = "cursor-model-catalog.json"

// cursorCatalogCacheKeep bounds the cached credentials so rotating a key does
// not grow the file without end.
const cursorCatalogCacheKeep = 8

// cursorCatalogRetrySchedule spaces out the attempts to recover a live catalog
// after a failed fetch, repeating the last interval until it succeeds.
var cursorCatalogRetrySchedule = []time.Duration{20 * time.Second, 45 * time.Second, 90 * time.Second, 3 * time.Minute, 5 * time.Minute}

func (s *Service) fetchCursorModelsForAuth(ctx context.Context, auth *coreauth.Auth) []*registry.ModelInfo {
	if auth == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	models, err := s.fetchCursorCatalogForAuth(ctx, auth)
	if err == nil && len(models) > 0 {
		s.cursorCatalogs().remember(auth.ID, cursorlib.SnapshotCatalog(models))
		log.Infof("cursor: AvailableModels loaded %d models for auth %s", len(models), auth.ID)
		return cursorlib.CatalogToModelInfos(models)
	}
	if err != nil {
		log.WithError(err).Warnf("cursor: model catalog unavailable for auth %s", auth.ID)
	}

	// A catalog fetch is the only thing that tells the gateway which models the
	// account can serve, so dropping to the builtin list on a transient upstream
	// failure would unregister every model the client was already using and turn
	// those requests into "unknown provider for model …". Serve the last good
	// catalog instead and keep retrying in the background.
	snapshot, source := s.cursorCatalogs().lookup(auth.ID)
	if snapshot == nil {
		log.Warnf("cursor: no cached model catalog for auth %s; using builtin model list", auth.ID)
		s.scheduleCursorCatalogRetry(auth.ID)
		return nil
	}
	cursorlib.RestoreCatalog(snapshot)
	log.Warnf("cursor: serving %d models from the %s catalog cached at %s for auth %s",
		len(snapshot.Models), source, snapshot.FetchedAt.UTC().Format(time.RFC3339), auth.ID)
	s.scheduleCursorCatalogRetry(auth.ID)
	return cursorlib.CatalogToModelInfos(snapshot.Models)
}

// fetchCursorCatalogForAuth exchanges the credential when needed and reads the
// account-visible model catalog from Cursor.
func (s *Service) fetchCursorCatalogForAuth(ctx context.Context, auth *coreauth.Auth) ([]cursorlib.CatalogModel, error) {
	creds := cursorlib.CredentialsFromMetadata(auth.Metadata)
	if strings.TrimSpace(creds.AccessToken) == "" {
		// API-key credentials have no stored token yet: exchange eagerly so
		// the live model catalog (not just the builtin fallback) registers.
		apiKey := cursorAuthAPIKey(auth)
		if apiKey == "" {
			return nil, nil
		}
		exchangeCtx, cancelExchange := context.WithTimeout(ctx, 25*time.Second)
		refreshed, err := cursorauth.NewAuthService().RefreshToken(exchangeCtx, apiKey, "", creds.BaseURL)
		cancelExchange()
		if err != nil {
			return nil, err
		}
		if auth.Metadata == nil {
			auth.Metadata = map[string]any{}
		}
		auth.Metadata["access_token"] = refreshed.AccessToken
		if !refreshed.ExpiresAt.IsZero() {
			auth.Metadata["expired"] = refreshed.ExpiresAt.UTC().Format(time.RFC3339)
		}
		creds = cursorlib.CredentialsFromMetadata(auth.Metadata)
		log.Infof("cursor: exchanged api key for agent token (auth %s)", auth.ID)
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	return cursorlib.FetchAvailableModels(fetchCtx, creds)
}

// cursorAuthAPIKey returns the configured Cursor user API key for an auth.
func cursorAuthAPIKey(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if key := strings.TrimSpace(auth.Attributes["api_key"]); key != "" {
			return key
		}
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["api_key"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// scheduleCursorCatalogRetry keeps polling Cursor for the catalog of one auth
// and re-registers its models as soon as a fetch succeeds, so a credential that
// started up during an upstream outage recovers without a config edit. At most
// one loop runs per auth.
func (s *Service) scheduleCursorCatalogRetry(authID string) {
	if s == nil || authID == "" {
		return
	}
	retry := s.cursorRetries()
	if retry == nil || !retry.begin(authID) {
		return
	}
	go func() {
		defer retry.done(authID)
		for attempt := 0; ; attempt++ {
			delay := cursorCatalogRetrySchedule[len(cursorCatalogRetrySchedule)-1]
			if attempt < len(cursorCatalogRetrySchedule) {
				delay = cursorCatalogRetrySchedule[attempt]
			}
			select {
			case <-retry.stop:
				return
			case <-time.After(delay):
			}

			auth, ok := s.latestAuthForModelRegistration(authID)
			if !ok || auth == nil || auth.Disabled {
				return
			}
			if _, err := s.fetchCursorCatalogForAuth(context.Background(), auth); err != nil {
				continue
			}
			log.Infof("cursor: model catalog reachable again; re-registering models for auth %s", authID)
			// The refresh re-runs the fetch and stores the fresh catalog.
			s.refreshModelRegistrationForAuth(auth)
			return
		}
	}()
}

func (s *Service) cursorCatalogs() *cursorCatalogStore {
	if s == nil {
		return newCursorCatalogStore("")
	}
	s.cursorCatalogOnce.Do(func() {
		dir := ""
		s.cfgMu.RLock()
		if s.cfg != nil {
			dir = strings.TrimSpace(s.cfg.AuthDir)
		}
		s.cfgMu.RUnlock()
		path := ""
		if dir != "" {
			path = filepath.Join(dir, cursorCatalogCacheFile)
		}
		s.cursorCatalogStore = newCursorCatalogStore(path)
	})
	return s.cursorCatalogStore
}

func (s *Service) cursorRetries() *cursorCatalogRetries {
	if s == nil {
		return nil
	}
	s.cursorRetryOnce.Do(func() {
		s.cursorCatalogRetries = &cursorCatalogRetries{
			inFlight: map[string]struct{}{},
			stop:     make(chan struct{}),
		}
	})
	return s.cursorCatalogRetries
}

// stopCursorCatalogRetries releases the background catalog pollers.
func (s *Service) stopCursorCatalogRetries() {
	if s == nil || s.cursorCatalogRetries == nil {
		return
	}
	s.cursorCatalogRetries.shutdown()
}

// cursorCatalogRetries tracks the running catalog pollers so a credential never
// gets more than one, and so they all end when the service shuts down.
type cursorCatalogRetries struct {
	mu       sync.Mutex
	inFlight map[string]struct{}
	stopped  bool
	stop     chan struct{}
}

func (r *cursorCatalogRetries) begin(authID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return false
	}
	if _, running := r.inFlight[authID]; running {
		return false
	}
	r.inFlight[authID] = struct{}{}
	return true
}

func (r *cursorCatalogRetries) done(authID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.inFlight, authID)
}

func (r *cursorCatalogRetries) shutdown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	r.stopped = true
	close(r.stop)
}

// cursorCatalogStore persists the last good AvailableModels response of every
// Cursor credential so a restart during an upstream outage still advertises the
// models the account actually serves.
type cursorCatalogStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	byAuth map[string]*cursorlib.CatalogSnapshot
}

type cursorCatalogCacheFileFormat struct {
	Auths map[string]*cursorlib.CatalogSnapshot `json:"auths"`
}

func newCursorCatalogStore(path string) *cursorCatalogStore {
	return &cursorCatalogStore{path: path, byAuth: map[string]*cursorlib.CatalogSnapshot{}}
}

// lookup returns the newest usable catalog for an auth. It prefers the entry the
// credential itself produced and otherwise falls back to the most recent catalog
// of another Cursor credential: every entry describes the same Cursor model
// lineup, so a freshly added key is far better served by a sibling's catalog
// than by the three-model builtin list.
func (c *cursorCatalogStore) lookup(authID string) (*cursorlib.CatalogSnapshot, string) {
	if c == nil {
		return nil, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	if snapshot := c.byAuth[authID]; snapshot != nil && len(snapshot.Models) > 0 {
		return snapshot, "own"
	}
	var newest *cursorlib.CatalogSnapshot
	for id, snapshot := range c.byAuth {
		if id == authID || snapshot == nil || len(snapshot.Models) == 0 {
			continue
		}
		if newest == nil || snapshot.FetchedAt.After(newest.FetchedAt) {
			newest = snapshot
		}
	}
	if newest == nil {
		return nil, ""
	}
	return newest, "sibling"
}

func (c *cursorCatalogStore) remember(authID string, snapshot *cursorlib.CatalogSnapshot) {
	if c == nil || authID == "" || snapshot == nil || len(snapshot.Models) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	c.byAuth[authID] = snapshot
	c.pruneLocked()
	c.saveLocked()
}

func (c *cursorCatalogStore) loadLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.path == "" {
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.WithError(err).Warnf("cursor: cannot read cached model catalog %s", c.path)
		}
		return
	}
	var parsed cursorCatalogCacheFileFormat
	if err = json.Unmarshal(raw, &parsed); err != nil {
		log.WithError(err).Warnf("cursor: ignoring malformed cached model catalog %s", c.path)
		return
	}
	for id, snapshot := range parsed.Auths {
		if id == "" || snapshot == nil || len(snapshot.Models) == 0 {
			continue
		}
		c.byAuth[id] = snapshot
	}
}

func (c *cursorCatalogStore) pruneLocked() {
	if len(c.byAuth) <= cursorCatalogCacheKeep {
		return
	}
	ids := make([]string, 0, len(c.byAuth))
	for id := range c.byAuth {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return c.byAuth[ids[i]].FetchedAt.After(c.byAuth[ids[j]].FetchedAt)
	})
	for _, id := range ids[cursorCatalogCacheKeep:] {
		delete(c.byAuth, id)
	}
}

func (c *cursorCatalogStore) saveLocked() {
	if c.path == "" {
		return
	}
	payload, err := json.MarshalIndent(cursorCatalogCacheFileFormat{Auths: c.byAuth}, "", "  ")
	if err != nil {
		log.WithError(err).Warn("cursor: cannot encode model catalog cache")
		return
	}
	tmp := c.path + ".tmp"
	if err = os.WriteFile(tmp, payload, 0o600); err != nil {
		log.WithError(err).Warnf("cursor: cannot write model catalog cache %s", c.path)
		return
	}
	if err = os.Rename(tmp, c.path); err != nil {
		log.WithError(err).Warnf("cursor: cannot replace model catalog cache %s", c.path)
		_ = os.Remove(tmp)
	}
}
