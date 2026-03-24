package mattermost

import (
	"sync"
	"time"
)

const stickyTTL = 30 * time.Minute

type stickyEntry struct {
	channelType string
	expiresAt   time.Time
}

// StickyRouter maps (channelID, userID) → agent channelType.
// Shared across all profiles so that @mentioning one bot clears
// the sticky for all others.
type StickyRouter struct {
	mu      sync.RWMutex
	entries map[string]stickyEntry
}

func NewStickyRouter() *StickyRouter {
	return &StickyRouter{entries: make(map[string]stickyEntry)}
}

func stickyKey(channelID, userID string) string { return channelID + "\x00" + userID }

func (r *StickyRouter) Set(channelID, userID, channelType string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[stickyKey(channelID, userID)] = stickyEntry{
		channelType: channelType,
		expiresAt:   time.Now().Add(stickyTTL),
	}
}

func (r *StickyRouter) Get(channelID, userID string) string {
	key := stickyKey(channelID, userID)
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()
	if !ok {
		return ""
	}
	if time.Now().After(e.expiresAt) {
		// Lazy eviction: upgrade to write lock and delete expired entry.
		r.mu.Lock()
		if e2, ok := r.entries[key]; ok && time.Now().After(e2.expiresAt) {
			delete(r.entries, key)
		}
		r.mu.Unlock()
		return ""
	}
	return e.channelType
}
