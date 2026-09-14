package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	ilink "github.com/pzx521521/openclaw-weixin-cli/internal/ilink"
)

var (
	ErrUserExists = errors.New("user already exists")
	ErrNotFound   = errors.New("not found")
)

const schema = `
CREATE TABLE IF NOT EXISTS wechat_users (
	bot_id          TEXT PRIMARY KEY,
	password_hash   TEXT NOT NULL,
	bot_token       TEXT NOT NULL,
	user_id         TEXT NOT NULL DEFAULT '',
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
	bot_id     TEXT NOT NULL REFERENCES wechat_users(bot_id) ON DELETE CASCADE,
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

// UserRow mirrors wechat_users.
type UserRow struct {
	BotID         string
	PasswordHash  string
	BotToken      string
	UserID        string
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
		&u.BotID, &u.PasswordHash, &u.BotToken, &u.UserID,
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

const userColumns = `bot_id, password_hash, bot_token, user_id, base_url, get_updates_buf, current_peer, peers, ready`

// CreateUser inserts a new account; returns ErrUserExists on duplicate bot_id.
func (s *Store) CreateUser(ctx context.Context, botID, passwordHash, botToken, userID, baseURL string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO wechat_users (bot_id, password_hash, bot_token, user_id, base_url) VALUES ($1,$2,$3,$4,$5)`,
		botID, passwordHash, botToken, userID, baseURL,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrUserExists
		}
		return err
	}
	return nil
}

// GetUser loads one account by bot_id.
func (s *Store) GetUser(ctx context.Context, botID string) (*UserRow, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM wechat_users WHERE bot_id=$1`, botID))
}

// ToState converts a row into the shared ilink session shape.
func (u *UserRow) ToState() *ilink.SessionState {
	peers := make(map[string]ilink.SessionPeer, len(u.Peers))
	for k, v := range u.Peers {
		peers[k] = v
	}
	return &ilink.SessionState{
		BotToken:      u.BotToken,
		BotID:         u.BotID,
		UserID:        u.UserID,
		BaseURL:       u.BaseURL,
		GetUpdatesBuf: u.GetUpdatesBuf,
		CurrentPeer:   u.CurrentPeer,
		Peers:         peers,
	}
}

// WithLockedState runs fn with the account row locked, then persists buf/peers.
func (s *Store) WithLockedState(ctx context.Context, botID string, fn func(state *ilink.SessionState, reg *ilink.ChatRegistry) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	u, err := scanUser(tx.QueryRow(ctx,
		`SELECT `+userColumns+` FROM wechat_users WHERE bot_id=$1 FOR UPDATE`, botID))
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
		`UPDATE wechat_users SET get_updates_buf=$2, current_peer=$3, peers=$4, ready=$5, updated_at=now() WHERE bot_id=$1`,
		botID, state.GetUpdatesBuf, state.CurrentPeer, string(peersRaw), ready,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateSession stores a login token with TTL and returns it.
func (s *Store) CreateSession(ctx context.Context, botID string, ttl time.Duration) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO wechat_sessions (token, bot_id, expires_at) VALUES ($1,$2,$3)`,
		token, botID, time.Now().Add(ttl),
	)
	if err != nil {
		return "", err
	}
	return token, nil
}

// GetSessionUser returns the bot_id for a valid, unexpired session token.
func (s *Store) GetSessionUser(ctx context.Context, token string) (string, error) {
	var botID string
	err := s.pool.QueryRow(ctx,
		`SELECT bot_id FROM wechat_sessions WHERE token=$1 AND expires_at>now()`, token).Scan(&botID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return botID, nil
}

// DeleteSession removes one login token.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM wechat_sessions WHERE token=$1`, token)
	return err
}

// CleanupExpired deletes expired login sessions.
func (s *Store) CleanupExpired(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM wechat_sessions WHERE expires_at<=now()`)
	return err
}
