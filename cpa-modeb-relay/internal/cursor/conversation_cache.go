package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v7/internal/cursor/proto/agent/v1"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"
)

// Cursor's provider-side prompt cache is tied to an upstream conversation.
// A follow-up request must replay the latest conversation checkpoint with the
// same conversation id; merely rebuilding identical root prompt blobs under a
// new id starts a cold provider conversation.
const (
	convCacheTTL     = 2 * time.Hour
	convCacheMaxSize = 2048
	convPendingWait  = checkpointGraceWindow + time.Second
)

type convEntry struct {
	conversationID string
	state          *agentv1.ConversationStateStructure
	blobs          map[string][]byte
	model          string
	expiresAt      time.Time
}

type pendingMarker struct {
	ch     chan struct{}
	closed bool
}

func (m *pendingMarker) closeLocked() {
	if !m.closed {
		m.closed = true
		close(m.ch)
	}
}

type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*convEntry
	pending map[string]*pendingMarker
}

var defaultConversationCache = &conversationCache{
	entries: map[string]*convEntry{},
	pending: map[string]*pendingMarker{},
}

func conversationReuseEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_CURSOR_CONV_REUSE"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func convCacheKey(scope, fingerprint string) string {
	return scope + "\x00" + fingerprint
}

// convScope prevents checkpoints for different models on the same account
// from overwriting each other. Cursor pins a conversation to its wire model,
// so a checkpoint created by one model cannot safely be resumed by another.
func convScope(accountKey, wireModelID string) string {
	if accountKey == "" || wireModelID == "" {
		return ""
	}
	return accountKey + "\x00" + wireModelID
}

func (c *conversationCache) Lookup(scope, fingerprint string) (*convEntry, bool) {
	if scope == "" || fingerprint == "" {
		return nil, false
	}
	key := convCacheKey(scope, fingerprint)
	entry, ok, wait := c.lookupOnce(key)
	if ok {
		return entry, true
	}
	if wait != nil {
		select {
		case <-wait:
		case <-time.After(convPendingWait):
		}
		if entry, ok, _ = c.lookupOnce(key); ok {
			return entry, true
		}
	}
	return c.loadPersisted(key)
}

func (c *conversationCache) LookupNoWait(scope, fingerprint string) (*convEntry, bool) {
	if scope == "" || fingerprint == "" {
		return nil, false
	}
	key := convCacheKey(scope, fingerprint)
	if entry, ok, _ := c.lookupOnce(key); ok {
		return entry, true
	}
	return c.loadPersisted(key)
}

func (c *conversationCache) PendingWait(scope, fingerprint string) chan struct{} {
	if scope == "" || fingerprint == "" {
		return nil
	}
	key := convCacheKey(scope, fingerprint)
	c.mu.Lock()
	defer c.mu.Unlock()
	if marker := c.pending[key]; marker != nil {
		return marker.ch
	}
	return nil
}

func (c *conversationCache) lookupOnce(key string) (*convEntry, bool, chan struct{}) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if ok {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
			return nil, false, nil
		}
		entry.expiresAt = now.Add(convCacheTTL)
		return entry, true, nil
	}
	if marker := c.pending[key]; marker != nil {
		return nil, false, marker.ch
	}
	return nil, false, nil
}

// BeginPending announces a checkpoint store before the response reaches the
// caller. Fast agent loops can issue their next request before Cursor sends the
// trailing checkpoint; those lookups wait briefly instead of becoming a miss.
func (c *conversationCache) BeginPending(scope, fingerprint string) func() {
	if scope == "" || fingerprint == "" {
		return func() {}
	}
	key := convCacheKey(scope, fingerprint)
	marker := &pendingMarker{ch: make(chan struct{})}
	c.mu.Lock()
	if previous := c.pending[key]; previous != nil {
		previous.closeLocked()
	}
	c.pending[key] = marker
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		if c.pending[key] == marker {
			delete(c.pending, key)
		}
		marker.closeLocked()
		c.mu.Unlock()
	}
}

func (c *conversationCache) Store(scope, fingerprint string, entry *convEntry) {
	if scope == "" || fingerprint == "" || entry == nil || entry.state == nil {
		return
	}
	entry.expiresAt = time.Now().Add(convCacheTTL)
	key := convCacheKey(scope, fingerprint)
	c.mu.Lock()
	if len(c.entries) >= convCacheMaxSize {
		c.pruneLocked()
	}
	c.entries[key] = entry
	c.mu.Unlock()
	persistConvEntry(key, entry)
}

func (c *conversationCache) Invalidate(scope, fingerprint string) {
	if scope == "" || fingerprint == "" {
		return
	}
	key := convCacheKey(scope, fingerprint)
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
	removePersistedConvEntry(key)
}

func (c *conversationCache) pruneLocked() {
	now := time.Now()
	for key, entry := range c.entries {
		if now.After(entry.expiresAt) {
			delete(c.entries, key)
		}
	}
	for len(c.entries) >= convCacheMaxSize {
		var oldestKey string
		var oldest time.Time
		for key, entry := range c.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = key, entry.expiresAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(c.entries, oldestKey)
	}
}

// Checkpoints are mirrored to disk so a deploy does not make every active
// conversation cold. The payload contains conversation text blobs and is
// written mode 0600; CPA_CURSOR_CONV_PERSIST=0 disables persistence.
type persistedConvEntry struct {
	ConversationID string            `json:"conversation_id"`
	Model          string            `json:"model"`
	ExpiresAtUnix  int64             `json:"expires_at_unix"`
	State          []byte            `json:"state"`
	Blobs          map[string][]byte `json:"blobs,omitempty"`
}

func convPersistEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPA_CURSOR_CONV_PERSIST"))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func convCacheDir() string {
	if !convPersistEnabled() {
		return ""
	}
	dir := strings.TrimSpace(os.Getenv("CPA_CURSOR_CONV_CACHE_DIR"))
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil || strings.TrimSpace(base) == "" {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "cliproxy", "cursor-conversations")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

func convEntryPath(dir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".ckpt")
}

func persistConvEntry(key string, entry *convEntry) {
	dir := convCacheDir()
	if dir == "" || entry == nil || entry.state == nil {
		return
	}
	state, err := proto.Marshal(entry.state)
	if err != nil {
		return
	}
	payload, err := json.Marshal(persistedConvEntry{
		ConversationID: entry.conversationID,
		Model:          entry.model,
		ExpiresAtUnix:  entry.expiresAt.Unix(),
		State:          state,
		Blobs:          entry.blobs,
	})
	if err != nil {
		return
	}
	path := convEntryPath(dir, key)
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, payload, 0o600); err != nil {
		return
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

func removePersistedConvEntry(key string) {
	if dir := convCacheDir(); dir != "" {
		_ = os.Remove(convEntryPath(dir, key))
	}
}

func (c *conversationCache) loadPersisted(key string) (*convEntry, bool) {
	dir := convCacheDir()
	if dir == "" {
		return nil, false
	}
	path := convEntryPath(dir, key)
	payload, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var stored persistedConvEntry
	if err = json.Unmarshal(payload, &stored); err != nil {
		_ = os.Remove(path)
		return nil, false
	}
	expiresAt := time.Unix(stored.ExpiresAtUnix, 0)
	if time.Now().After(expiresAt) || strings.TrimSpace(stored.ConversationID) == "" {
		_ = os.Remove(path)
		return nil, false
	}
	state := &agentv1.ConversationStateStructure{}
	if err = proto.Unmarshal(stored.State, state); err != nil {
		_ = os.Remove(path)
		return nil, false
	}
	entry := &convEntry{
		conversationID: stored.ConversationID,
		state:          state,
		blobs:          stored.Blobs,
		model:          stored.Model,
		expiresAt:      expiresAt,
	}
	c.mu.Lock()
	if existing := c.entries[key]; existing != nil {
		c.mu.Unlock()
		return existing, true
	}
	if len(c.entries) >= convCacheMaxSize {
		c.pruneLocked()
	}
	c.entries[key] = entry
	c.mu.Unlock()
	log.Debugf("cursor: restored conversation checkpoint from disk conv=%s model=%s", entry.conversationID, entry.model)
	return entry, true
}

func sweepPersistedConvEntries() {
	dir := convCacheDir()
	if dir == "" {
		return
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".ckpt") {
			continue
		}
		path := filepath.Join(dir, file.Name())
		payload, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		var stored persistedConvEntry
		if json.Unmarshal(payload, &stored) != nil || now.After(time.Unix(stored.ExpiresAtUnix, 0)) {
			_ = os.Remove(path)
		}
	}
}

func init() {
	go sweepPersistedConvEntries()
}

// accountKeyForSession prefers the gateway auth id because access tokens
// rotate. Direct library callers fall back to stable account metadata.
func accountKeyForSession(authID string, creds AccountCredentials) string {
	if value := strings.TrimSpace(authID); value != "" {
		return "auth:" + value
	}
	if value := strings.TrimSpace(creds.Email); value != "" {
		return "email:" + strings.ToLower(value)
	}
	for _, candidate := range []struct {
		label string
		value string
	}{
		{label: "refresh", value: creds.RefreshToken},
		{label: "access", value: creds.AccessToken},
	} {
		if value := strings.TrimSpace(candidate.value); value != "" {
			sum := sha256.Sum256([]byte(value))
			return candidate.label + ":" + hex.EncodeToString(sum[:8])
		}
	}
	if value := strings.TrimSpace(creds.MachineID); value != "" {
		return "machine:" + value
	}
	return ""
}

func conversationFingerprint(messages []ChatMessage) string {
	h := sha256.New()
	for i := range messages {
		fingerprintMessage(h, &messages[i])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func conversationPrefixFingerprints(messages []ChatMessage) []string {
	h := sha256.New()
	out := make([]string, len(messages)+1)
	out[0] = hex.EncodeToString(h.Sum(nil))
	for i := range messages {
		fingerprintMessage(h, &messages[i])
		out[i+1] = hex.EncodeToString(h.Sum(nil))
	}
	return out
}

func fingerprintMessage(h hash.Hash, msg *ChatMessage) {
	role := strings.TrimSpace(msg.Role)
	content := strings.TrimSpace(msg.Content)
	if role == "" || (content == "" && len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.ToolCallID) == "") {
		return
	}
	h.Write([]byte{0x1e})
	h.Write([]byte(role))
	h.Write([]byte{0x1f})
	h.Write([]byte(content))
	h.Write([]byte{0x1f})
	if role == "tool" {
		h.Write([]byte(normalizeFingerprintToolID(msg.ToolCallID)))
		h.Write([]byte{0x1f})
	}
	for _, call := range msg.ToolCalls {
		h.Write([]byte(normalizeFingerprintToolID(call.ID)))
		h.Write([]byte{0x1f})
		h.Write([]byte(strings.TrimSpace(call.Name)))
		h.Write([]byte{0x1f})
		if len(call.Arguments) > 0 {
			if payload, err := json.Marshal(call.Arguments); err == nil {
				h.Write(payload)
			}
		}
		h.Write([]byte{0x1f})
	}
}

func normalizeFingerprintToolID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	return util.SanitizeClaudeToolID(id)
}
