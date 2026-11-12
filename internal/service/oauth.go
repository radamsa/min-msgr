package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
)

// OAuthService maps OAuth identities to local users. The first successful
// login registers the user; later logins reuse the same account.
type OAuthService struct {
	pool *pgxpool.Pool
}

func NewOAuthService(pool *pgxpool.Pool) *OAuthService {
	return &OAuthService{pool: pool}
}

// Login returns the user bound to a provider account or creates one. Concurrent
// first logins are resolved by the unique (provider, provider_user_id) index.
func (s *OAuthService) Login(
	ctx context.Context,
	provider string,
	providerUserID string,
	login string,
	email string,
) (User, error) {
	provider = strings.TrimSpace(provider)
	providerUserID = strings.TrimSpace(providerUserID)
	login = strings.TrimSpace(login)
	if provider == "" || providerUserID == "" {
		return User{}, fmt.Errorf("provider and provider user id must not be empty")
	}
	if login == "" || len(login) > 255 {
		return User{}, fmt.Errorf("login must be 1-255 characters")
	}

	for attempt := 0; attempt < 5; attempt++ {
		user, found, err := s.findUser(ctx, provider, providerUserID)
		if err != nil {
			return User{}, err
		}
		if found {
			return user, nil
		}

		user, err = s.createAccount(ctx, provider, providerUserID, login, email)
		if err == nil {
			return user, nil
		}
		if isUniqueViolation(err) {
			continue // another request won the registration race
		}
		return User{}, fmt.Errorf("create oauth account: %w", err)
	}
	return User{}, fmt.Errorf("create oauth account: too many registration races")
}

func (s *OAuthService) findUser(ctx context.Context, provider, providerUserID string) (User, bool, error) {
	var user User
	var blocked bool
	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.public_id, u.oauth_login, u.is_admin, u.is_blocked
		FROM oauth_accounts a
		JOIN users u ON u.id = a.user_id
		WHERE a.provider = $1 AND a.provider_user_id = $2
	`, provider, providerUserID).Scan(
		&user.ID, &user.PublicID, &user.Login, &user.IsAdmin, &blocked,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, fmt.Errorf("find oauth account: %w", err)
	}
	if blocked {
		return User{}, false, domain.ErrBlocked
	}
	return user, true, nil
}

func (s *OAuthService) createAccount(
	ctx context.Context,
	provider string,
	providerUserID string,
	login string,
	email string,
) (User, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("begin oauth registration: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	publicID, err := newPublicID(login)
	if err != nil {
		return User{}, err
	}

	var emailValue any
	if value := strings.TrimSpace(email); value != "" {
		emailValue = value
	}

	var user User
	err = tx.QueryRow(ctx, `
		INSERT INTO users (oauth_login, public_id, email)
		VALUES ($1, $2, $3)
		RETURNING id, public_id, oauth_login, is_admin
	`, login, publicID, emailValue).Scan(&user.ID, &user.PublicID, &user.Login, &user.IsAdmin)
	if err != nil {
		return User{}, err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO oauth_accounts (user_id, provider, provider_user_id, email)
		VALUES ($1, $2, $3, $4)
	`, user.ID, provider, providerUserID, emailValue); err != nil {
		return User{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("commit oauth registration: %w", err)
	}
	return user, nil
}
