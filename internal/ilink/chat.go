package ilink

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChatRegistry tracks the latest reply context for each peer.
type ChatRegistry struct {
	mu      sync.RWMutex
	current string
	peers   map[string]SessionPeer
}

// NewChatRegistry builds the in-memory chat state from persisted session data.
func NewChatRegistry(state *SessionState) *ChatRegistry {
	peers := make(map[string]SessionPeer)
	current := ""
	if state != nil {
		for peer, saved := range state.Peers {
			peer = strings.TrimSpace(peer)
			saved.ContextToken = strings.TrimSpace(saved.ContextToken)
			saved.LastSeenAt = strings.TrimSpace(saved.LastSeenAt)
			if peer == "" || saved.ContextToken == "" {
				continue
			}
			peers[peer] = saved
		}
		current = strings.TrimSpace(state.CurrentPeer)
		if current != "" {
			if _, ok := peers[current]; !ok {
				current = ""
			}
		}
	}
	return &ChatRegistry{
		current: current,
		peers:   peers,
	}
}

// Upsert records the latest context token for one peer.
func (r *ChatRegistry) Upsert(peer, contextToken string, seenAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if peer == "" {
		return
	}

	entry := r.peers[peer]
	if contextToken != "" {
		entry.ContextToken = contextToken
	}
	if !seenAt.IsZero() {
		entry.LastSeenAt = seenAt.Format(time.RFC3339)
	}
	r.peers[peer] = entry
	if r.current == "" {
		r.current = peer
	}
}

// SetCurrent switches the active peer used by plain text input.
func (r *ChatRegistry) SetCurrent(peer string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.peers[peer]; !ok {
		return fmt.Errorf("unknown peer: %s", peer)
	}
	r.current = peer
	return nil
}

// Current returns the selected peer and its context token.
func (r *ChatRegistry) Current() (string, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.current == "" {
		return "", "", false
	}
	peer, ok := r.peers[r.current]
	return r.current, peer.ContextToken, ok
}

// List returns the known peers in stable order.
func (r *ChatRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	peers := make([]string, 0, len(r.peers))
	for peer := range r.peers {
		peers = append(peers, peer)
	}
	sort.Slice(peers, func(i, j int) bool {
		left := r.peers[peers[i]].LastSeenAt
		right := r.peers[peers[j]].LastSeenAt
		if left == right {
			return peers[i] < peers[j]
		}
		return left > right
	})
	return peers
}

// Token returns the latest context token for one peer.
func (r *ChatRegistry) Token(peer string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	entry, ok := r.peers[peer]
	return entry.ContextToken, ok
}

// LastSeenAt returns the last inbound time recorded for one peer.
func (r *ChatRegistry) LastSeenAt(peer string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.peers[peer].LastSeenAt
}

// ApplyToState copies the current registry snapshot into the persisted session.
func (r *ChatRegistry) ApplyToState(state *SessionState) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	state.CurrentPeer = r.current
	state.Peers = make(map[string]SessionPeer, len(r.peers))
	for peer, saved := range r.peers {
		state.Peers[peer] = saved
	}
}

// PersistState saves session and cached users without letting concurrent writes race.
func PersistState(statePath string, state *SessionState, registry *ChatRegistry, persistMu *sync.Mutex) error {
	persistMu.Lock()
	defer persistMu.Unlock()

	registry.ApplyToState(state)
	return SaveState(statePath, state)
}

// ExtractMessageText picks the first text segment from an inbound message.
func ExtractMessageText(msg WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == 1 && item.TextItem != nil {
			return strings.TrimSpace(item.TextItem.Text)
		}
		if item.Type == 3 && item.VoiceItem != nil {
			return strings.TrimSpace(item.VoiceItem.Text)
		}
	}
	return ""
}

// SummarizeMessage provides a readable fallback for non-text messages.
func SummarizeMessage(msg WeixinMessage) string {
	if len(msg.ItemList) == 0 {
		return "[empty message]"
	}

	kinds := make([]string, 0, len(msg.ItemList))
	for _, item := range msg.ItemList {
		switch item.Type {
		case 1:
			kinds = append(kinds, "text")
		case 2:
			kinds = append(kinds, "image")
		case 3:
			kinds = append(kinds, "voice")
		case 4:
			kinds = append(kinds, "file")
		case 5:
			kinds = append(kinds, "video")
		default:
			kinds = append(kinds, fmt.Sprintf("type-%d", item.Type))
		}
	}
	return "[" + strings.Join(kinds, ", ") + "]"
}
