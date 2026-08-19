package cursor

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// conversationReuseTTL bounds how long a conversation id stays reusable.
	// Upstream prompt caches are far shorter lived, but keeping the mapping
	// around costs nothing and avoids splitting a slow human conversation.
	conversationReuseTTL = 3 * time.Hour
	// conversationRegistryMax caps the registry so a busy gateway cannot grow
	// it without bound; the oldest entries are dropped first.
	conversationRegistryMax = 8192
	// conversationMinPrefix is the shortest prefix eligible for reuse. A bare
	// system row is shared by every fresh conversation, so matching on it
	// would collapse unrelated conversations onto one id.
	conversationMinPrefix = 2
)

// conversationRegistry maps a conversation's message-prefix fingerprint to the
// Cursor conversation id it last ran under.
//
// Cursor derives the cacheable head of the upstream prompt from the
// conversation the run belongs to, so minting a fresh uuid per request (the
// previous behaviour) made every turn look like a brand new conversation and
// discarded the provider prompt cache. Because the prompt blobs are
// content-addressed, turn N's fingerprint is always a prefix of turn N+1's,
// which lets a follow-up turn recover the id its predecessor used.
type conversationRegistry struct {
	mu      sync.Mutex
	entries map[string]conversationEntry
}

type conversationEntry struct {
	id       string
	lastUsed time.Time
}

var defaultConversationRegistry = &conversationRegistry{entries: map[string]conversationEntry{}}

// conversationPrefixKeys returns one rolling fingerprint per prefix of ids,
// shortest first; element i covers ids[0..i].
func conversationPrefixKeys(scope string, ids [][]byte) []string {
	if len(ids) == 0 {
		return nil
	}
	keys := make([]string, 0, len(ids))
	rolling := sha256.Sum256([]byte("cursor-conversation\x00" + scope))
	for _, id := range ids {
		h := sha256.New()
		h.Write(rolling[:])
		h.Write(id)
		copy(rolling[:], h.Sum(nil))
		keys = append(keys, hex.EncodeToString(rolling[:]))
	}
	return keys
}

// resolve returns the conversation id to run this prompt under, reusing the id
// of the longest known ancestor prefix and minting a new one otherwise. The
// full prefix is always (re)registered so the next turn matches immediately.
func (r *conversationRegistry) resolve(scope string, ids [][]byte) string {
	keys := conversationPrefixKeys(scope, ids)
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	id := ""
	for i := len(keys) - 1; i >= conversationMinPrefix-1; i-- {
		entry, ok := r.entries[keys[i]]
		if !ok {
			continue
		}
		if now.Sub(entry.lastUsed) > conversationReuseTTL {
			delete(r.entries, keys[i])
			continue
		}
		id = entry.id
		break
	}
	if id == "" {
		id = uuid.NewString()
	}
	if len(keys) >= conversationMinPrefix {
		r.entries[keys[len(keys)-1]] = conversationEntry{id: id, lastUsed: now}
	}
	r.pruneLocked(now)
	return id
}

func (r *conversationRegistry) pruneLocked(now time.Time) {
	if len(r.entries) <= conversationRegistryMax {
		return
	}
	for key, entry := range r.entries {
		if now.Sub(entry.lastUsed) > conversationReuseTTL {
			delete(r.entries, key)
		}
	}
	// Still oversized: drop arbitrary entries until back under the cap. Losing
	// a mapping only costs one uncached turn, so exact LRU is not worth the
	// bookkeeping.
	for key := range r.entries {
		if len(r.entries) <= conversationRegistryMax {
			break
		}
		delete(r.entries, key)
	}
}
