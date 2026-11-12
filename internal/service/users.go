package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
)

// User is a local user.
type User struct {
	ID       int64
	PublicID string
	Login    string
	IsAdmin  bool
}

type UserService struct {
	pool *pgxpool.Pool
}

func NewUserService(pool *pgxpool.Pool) *UserService {
	return &UserService{pool: pool}
}

// Create registers a user on first successful OAuth login. The public ID gets
// a random three-digit discriminator so equal OAuth logins stay distinct.
func (s *UserService) Create(ctx context.Context, login, email string) (User, error) {
	login = strings.TrimSpace(login)
	if login == "" || len(login) > 255 {
		return User{}, fmt.Errorf("login must be 1-255 characters")
	}

	var emailValue any
	if value := strings.TrimSpace(email); value != "" {
		emailValue = value
	}

	for attempt := 0; attempt < 10; attempt++ {
		publicID, err := newPublicID(login)
		if err != nil {
			return User{}, err
		}

		var user User
		err = s.pool.QueryRow(ctx, `
			INSERT INTO users (oauth_login, public_id, email)
			VALUES ($1, $2, $3)
			RETURNING id, public_id, oauth_login, is_admin
		`, login, publicID, emailValue).Scan(&user.ID, &user.PublicID, &user.Login, &user.IsAdmin)
		if err == nil {
			return user, nil
		}
		if isUniqueViolation(err) {
			continue
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return User{}, fmt.Errorf("create user: could not allocate a unique public id")
}

// ByPublicID returns the user with the given public ID ("login#NNN").
func (s *UserService) ByPublicID(ctx context.Context, publicID string) (User, error) {
	return s.byColumn(ctx, "public_id", publicID)
}

// ByID returns the user with the given numeric ID.
func (s *UserService) ByID(ctx context.Context, id int64) (User, error) {
	var user User
	err := s.pool.QueryRow(ctx, `
		SELECT id, public_id, oauth_login, is_admin FROM users WHERE id = $1
	`, id).Scan(&user.ID, &user.PublicID, &user.Login, &user.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, domain.ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by id: %w", err)
	}
	return user, nil
}

// ByLogin returns all users with the given OAuth login. Equal logins are
// allowed; the public ID keeps them distinct.
func (s *UserService) ByLogin(ctx context.Context, login string) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, public_id, oauth_login, is_admin FROM users WHERE oauth_login = $1
	`, login)
	if err != nil {
		return nil, fmt.Errorf("find users by login: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.ID, &user.PublicID, &user.Login, &user.IsAdmin); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

func (s *UserService) byColumn(ctx context.Context, column, value string) (User, error) {
	var user User
	err := s.pool.QueryRow(ctx, `
		SELECT id, public_id, oauth_login, is_admin
		FROM users WHERE `+column+` = $1
	`, value).Scan(&user.ID, &user.PublicID, &user.Login, &user.IsAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, domain.ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("find user by %s: %w", column, err)
	}
	return user, nil
}

func newPublicID(login string) (string, error) {
	value, err := rand.Int(rand.Reader, big.NewInt(1000))
	if err != nil {
		return "", fmt.Errorf("generate public id: %w", err)
	}
	return fmt.Sprintf("%s#%03d", login, value.Int64()), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
