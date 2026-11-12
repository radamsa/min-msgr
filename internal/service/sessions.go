package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
)

// SessionService issues and validates browser session tokens. Only SHA-256
// hashes are stored, so a database leak does not expose live sessions.
type SessionService struct {
	pool *pgxpool.Pool
	ttl  time.Duration
}

func NewSessionService(pool *pgxpool.Pool, ttl time.Duration) *SessionService {
	return &SessionService{pool: pool, ttl: ttl}
}

// Create issues a new session for the user and returns the plaintext token.
func (s *SessionService) Create(ctx context.Context, userID int64) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	expiresAt := time.Now().UTC().Add(s.ttl)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)
	`, hash[:], userID, expiresAt); err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	return token, expiresAt, nil
}

// Authenticate resolves a session token to its user. Expired sessions and
// blocked users are rejected.
func (s *SessionService) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, domain.ErrUnauthorized
	}
	hash := sha256.Sum256([]byte(token))

	var user User
	var blocked bool
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.public_id, u.oauth_login, u.is_admin, u.is_blocked
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()
	`, hash[:]).Scan(&user.ID, &user.PublicID, &user.Login, &user.IsAdmin, &blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, domain.ErrUnauthorized
	}
	if err != nil {
		return User{}, fmt.Errorf("authenticate session: %w", err)
	}
	if blocked {
		return User{}, domain.ErrUnauthorized
	}
	return user, nil
}

// Delete invalidates a session. Missing sessions are not an error.
func (s *SessionService) Delete(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	hash := sha256.Sum256([]byte(token))
	if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hash[:]); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}
