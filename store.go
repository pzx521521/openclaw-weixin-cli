package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SessionState persists the login token and long-poll cursor.
type SessionPeer struct {
	ContextToken string `json:"context_token"`
	LastSeenAt   string `json:"last_seen_at,omitempty"`
}

// SessionState persists the login token, long-poll cursor, and known peers.
type SessionState struct {
	BotToken      string                 `json:"bot_token"`
	BotID         string                 `json:"bot_id"`
	UserID        string                 `json:"user_id"`
	BaseURL       string                 `json:"base_url"`
	GetUpdatesBuf string                 `json:"get_updates_buf"`
	CurrentPeer   string                 `json:"current_peer,omitempty"`
	Peers         map[string]SessionPeer `json:"peers,omitempty"`
	SavedAt       string                 `json:"saved_at"`
}

type legacySessionState struct {
	BotToken      string            `json:"bot_token"`
	BotID         string            `json:"bot_id"`
	UserID        string            `json:"user_id"`
	BaseURL       string            `json:"base_url"`
	GetUpdatesBuf string            `json:"get_updates_buf"`
	CurrentPeer   string            `json:"current_peer,omitempty"`
	Peers         map[string]string `json:"peers,omitempty"`
	SavedAt       string            `json:"saved_at"`
}

// LoadState reads the state file from disk.
func LoadState(path string) (*SessionState, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var state SessionState
	if err := json.Unmarshal(raw, &state); err != nil {
		var legacy legacySessionState
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return nil, err
		}
		state = SessionState{
			BotToken:      legacy.BotToken,
			BotID:         legacy.BotID,
			UserID:        legacy.UserID,
			BaseURL:       legacy.BaseURL,
			GetUpdatesBuf: legacy.GetUpdatesBuf,
			CurrentPeer:   legacy.CurrentPeer,
			SavedAt:       legacy.SavedAt,
			Peers:         make(map[string]SessionPeer, len(legacy.Peers)),
		}
		for peer, token := range legacy.Peers {
			state.Peers[peer] = SessionPeer{ContextToken: token}
		}
	}
	state.BaseURL = normalizeBaseURL(state.BaseURL)
	if state.Peers == nil {
		state.Peers = make(map[string]SessionPeer)
	}
	return &state, nil
}

// HasUsableSession reports whether the saved session can be used for chat mode.
func HasUsableSession(path string) bool {
	state, err := LoadState(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(state.BotToken) != ""
}

// SaveState writes the current state file atomically.
func SaveState(path string, state *SessionState) error {
	if state == nil {
		return errors.New("state is nil")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	state.BaseURL = normalizeBaseURL(state.BaseURL)
	if state.Peers == nil {
		state.Peers = make(map[string]SessionPeer)
	}
	state.SavedAt = time.Now().Format(time.RFC3339)

	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// normalizeBaseURL keeps the saved base URL usable across restarts.
func normalizeBaseURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(baseURL, "/")
}
