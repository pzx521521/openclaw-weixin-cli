package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ilink "github.com/pzx521521/openclaw-weixin-cli/internal/ilink"
)

var (
	ErrNotFound = errors.New("not found")
)

const schema = `
CREATE TABLE IF NOT EXISTS wechat_users (
	user_id         TEXT PRIMARY KEY,
	password_hash   TEXT NOT NULL,
	bot_token       TEXT NOT NULL,
	base_url        TEXT NOT NULL DEFAULT 'https://ilinkai.weixin.qq.com',
	get_updates_buf TEXT NOT NULL DEFAULT '',
	current_peer    TEXT NOT NULL DEFAULT '',
	peers           JSONB NOT NULL DEFAULT '{}',
	ready           BOOLEAN NOT NULL DEFAULT FALSE,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS wechat_sessions (
	token      TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL REFERENCES wechat_users(user_id) ON DELETE CASCADE,
	expires_at TIMESTAMPTZ NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS wechat_sessions_expires_idx ON wechat_sessions(expires_at);
`

// Store persists users and login sessions in Postgres.
type Store struct {
	pool *pgxpool.Pool
}

// Connect opens a pgx pool and pings the database.
func Connect(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect pg: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping pg: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() {
	s.pool.Close()
}

// Ping checks the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Migrate creates tables when missing.
func (s *Store) Migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schema)
	return err
}

// UserRow mirrors wechat_users, keyed by WeChat user_id.
type UserRow struct {
	UserID        string
	PasswordHash  string
	BotToken      string
	BaseURL       string
	GetUpdatesBuf string
	CurrentPeer   string
	Peers         map[string]ilink.SessionPeer
	Ready         bool
}

func scanUser(row pgx.Row) (*UserRow, error) {
	var u UserRow
	var peersRaw string
	err := row.Scan(
		&u.UserID, &u.PasswordHash, &u.BotToken,
		&u.BaseURL, &u.GetUpdatesBuf, &u.CurrentPeer,
		&peersRaw, &u.Ready,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.Peers = make(map[string]ilink.SessionPeer)
	if peersRaw != "" {
		if err := json.Unmarshal([]byte(peersRaw), &u.Peers); err != nil {
			return nil, fmt.Errorf("decode peers: %w", err)
		}
	}
	return &u, nil
}

const userColumns = `user_id, password_hash, bot_token, base_url, get_updates_buf, current_peer, peers, ready`

// UpsertUserBinding overwrites the row for one WeChat identity with a fresh
// scan binding: credentials replaced, sync state reset, previous sessions
// dropped. Returns overwritten=true when the identity already existed.
func (s *Store) UpsertUserBinding(ctx context.Context, userID, passwordHash, botToken, baseURL string) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wechat_users WHERE user_id=$1)`, userID).Scan(&exists); err != nil {
		return false, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO wechat_users (user_id, password_hash, bot_token, base_url,
			get_updates_buf, current_peer, peers, ready, updated_at)
		VALUES ($1,$2,$3,$4,'','','{}',FALSE,now())
		ON CONFLICT (user_id) DO UPDATE SET
			password_hash=EXCLUDED.password_hash,
			bot_token=EXCLUDED.bot_token,
			base_url=EXCLUDED.base_url,
			get_updates_buf='',
			current_peer='',
			peers='{}',
			ready=FALSE,
			updated_at=now()`,
		userID, passwordHash, botToken, baseURL,
	); err != nil {
		return false, err
	}
	// Credentials rotated: drop previous sessions of this identity.
	if _, err := tx.Exec(ctx, `DELETE FROM wechat_sessions WHERE user_id=$1`, userID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return exists, nil
}

// GetUser loads one account by WeChat user_id (the login name).
func (s *Store) GetUser(ctx context.Context, userID string) (*UserRow, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, ErrNotFound
	}
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM wechat_users WHERE user_id=$1`, strings.TrimSpace(userID)))
}

// ToState converts a row into the shared ilink session shape.
func (u *UserRow) ToState() *ilink.SessionState {
	peers := make(map[string]ilink.SessionPeer, len(u.Peers))
	for k, v := range u.Peers {
		peers[k] = v
	}
	return &ilink.SessionState{
		BotToken:      u.BotToken,
		UserID:        u.UserID,
		BaseURL:       u.BaseURL,
		GetUpdatesBuf: u.GetUpdatesBuf,
		CurrentPeer:   u.CurrentPeer,
		Peers:         peers,
	}
}

// WithLockedState runs fn with the account row locked, then persists buf/peers.
func (s *Store) WithLockedState(ctx context.Context, userID string, fn func(state *ilink.SessionState, reg *ilink.ChatRegistry) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	u, err := scanUser(tx.QueryRow(ctx,
		`SELECT `+userColumns+` FROM wechat_users WHERE user_id=$1 FOR UPDATE`, userID))
	if err != nil {
		return err
	}

	state := u.ToState()
	reg := ilink.NewChatRegistry(state)
	if err := fn(state, reg); err != nil {
		return err
	}
	reg.ApplyToState(state)

	peersRaw, err := json.Marshal(state.Peers)
	if err != nil {
		return err
	}
	ready := u.Ready || len(state.Peers) > 0
	_, err = tx.Exec(ctx,
		`UPDATE wechat_users SET get_updates_buf=$2, current_peer=$3, peers=$4, ready=$5, updated_at=now() WHERE user_id=$1`,
		u.UserID, state.GetUpdatesBuf, state.CurrentPeer, string(peersRaw), ready,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateSession stores a login token with TTL and returns it.
func (s *Store) CreateSession(ctx context.Context, userID string, ttl time.Duration) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO wechat_sessions (token, user_id, expires_at) VALUES ($1,$2,$3)`,
		token, userID, time.Now().Add(ttl),
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// GetSessionUser returns the user_id for a valid, unexpired session token.
func (s *Store) GetSessionUser(ctx context.Context, token string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM wechat_sessions WHERE token=$1 AND expires_at>now()`, token).Scan(&userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return userID, nil
}

// DeleteSession removes one login token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM wechat_sessions WHERE token=$1`, token)
	return err
}

// DeleteUserSessions removes all login tokens of one account.
// Called on re-login so old cookies stop working (single active session).
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM wechat_sessions WHERE user_id=$1`, userID)
	return err
}

// CleanupExpired deletes expired login sessions.
func (s *Store) CleanupExpired(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM wechat_sessions WHERE expires_at<=now()`)
	return err
}
