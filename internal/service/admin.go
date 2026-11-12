package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
)

// AdminSettingsView holds the global defaults shown and edited in the admin
// panel.
type AdminSettingsView struct {
	MaxKeysPerUser       int
	MaxDaysLimit         int
	MaxBytesPerChat      int64
	MaxTotalBytesPerUser int64
}

// UserListItem is one user in the admin user list. Admins never see chat
// contents, only structure.
type UserListItem struct {
	ID        int64
	PublicID  string
	Login     string
	IsAdmin   bool
	IsBlocked bool
	ChatCount int64
}

// UserOverrideItem is one stored per-user override.
type UserOverrideItem struct {
	UserID               int64
	MaxKeysPerUser       *int
	MaxDaysLimit         *int
	MaxBytesPerChat      *int64
	MaxTotalBytesPerUser *int64
}

// AdminService implements the admin panel: global defaults, per-user
// overrides, and user management. It never exposes chat contents.
type AdminService struct {
	pool *pgxpool.Pool
}

func NewAdminService(pool *pgxpool.Pool) *AdminService {
	return &AdminService{pool: pool}
}

// Settings returns the global defaults.
func (s *AdminService) Settings(ctx context.Context) (AdminSettingsView, error) {
	var view AdminSettingsView
	err := s.pool.QueryRow(ctx, `
		SELECT max_keys_per_user, max_days_limit, max_bytes_per_chat,
			max_total_bytes_per_user
		FROM admin_settings WHERE id = TRUE
	`).Scan(
		&view.MaxKeysPerUser, &view.MaxDaysLimit,
		&view.MaxBytesPerChat, &view.MaxTotalBytesPerUser,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminSettingsView{}, fmt.Errorf("admin settings are missing")
	}
	if err != nil {
		return AdminSettingsView{}, fmt.Errorf("read admin settings: %w", err)
	}
	return view, nil
}

// UpdateSettings replaces the global defaults. Invalid values are rejected
// here and by the database CHECK constraints.
func (s *AdminService) UpdateSettings(
	ctx context.Context,
	maxKeys, maxDays int,
	maxBytesPerChat, maxTotalBytes int64,
) (AdminSettingsView, error) {
	if maxKeys <= 0 || maxDays <= 0 || maxBytesPerChat <= 0 || maxTotalBytes <= 0 {
		return AdminSettingsView{}, fmt.Errorf("all limits must be positive")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE admin_settings
		SET max_keys_per_user = $1, max_days_limit = $2,
			max_bytes_per_chat = $3, max_total_bytes_per_user = $4,
			updated_at = now()
		WHERE id = TRUE
	`, maxKeys, maxDays, maxBytesPerChat, maxTotalBytes)
	if err != nil {
		return AdminSettingsView{}, fmt.Errorf("update admin settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return AdminSettingsView{}, fmt.Errorf("admin settings are missing")
	}
	return s.Settings(ctx)
}

// Users lists all users with their chat counts.
func (s *AdminService) Users(ctx context.Context) ([]UserListItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.public_id, u.oauth_login, u.is_admin, u.is_blocked,
			count(c.id)::bigint AS chat_count
		FROM users u
		LEFT JOIN chats c ON c.user_id = u.id
		GROUP BY u.id
		ORDER BY u.id
	`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []UserListItem
	for rows.Next() {
		var item UserListItem
		if err := rows.Scan(
			&item.ID, &item.PublicID, &item.Login,
			&item.IsAdmin, &item.IsBlocked, &item.ChatCount,
		); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

// SetBlocked blocks or unblocks a user. Blocked users cannot log in; existing
// sessions stop working immediately and API keys are rejected.
func (s *AdminService) SetBlocked(ctx context.Context, userID int64, blocked bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE users SET is_blocked = $1 WHERE id = $2
	`, blocked, userID)
	if err != nil {
		return fmt.Errorf("update user block status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	if blocked {
		if _, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("delete sessions of blocked user: %w", err)
		}
	}
	return nil
}

// DeleteUser removes a user with all their chats, keys, messages, sessions,
// and overrides (via ON DELETE CASCADE). Archives reference the user and are
// removed together with their rows; archive files under ARCHIVES_DIR are the
// retention cron's responsibility.
func (s *AdminService) DeleteUser(ctx context.Context, userID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// Overrides lists stored per-user overrides with the user's public ID.
func (s *AdminService) Overrides(ctx context.Context) ([]UserOverrideItem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT uo.user_id, u.public_id,
			uo.max_keys_per_user, uo.max_days_limit,
			uo.max_bytes_per_chat, uo.max_total_bytes_per_user
		FROM user_overrides uo
		JOIN users u ON u.id = uo.user_id
		ORDER BY uo.user_id
	`)
	if err != nil {
		return nil, fmt.Errorf("list overrides: %w", err)
	}
	defer rows.Close()

	var items []UserOverrideItem
	for rows.Next() {
		var item UserOverrideItem
		var publicID string
		if err := rows.Scan(
			&item.UserID, &publicID,
			&item.MaxKeysPerUser, &item.MaxDaysLimit,
			&item.MaxBytesPerChat, &item.MaxTotalBytesPerUser,
		); err != nil {
			return nil, fmt.Errorf("scan override: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate overrides: %w", err)
	}
	return items, nil
}

// SetOverride stores one per-user override; a nil value drops that field back
// to the global default. Invalid positive values are rejected.
func (s *AdminService) SetOverride(
	ctx context.Context,
	userID int64,
	maxKeys, maxDays *int,
	maxBytesPerChat, maxTotalBytes *int64,
) error {
	for _, v := range []*int{maxKeys, maxDays} {
		if v != nil && *v <= 0 {
			return fmt.Errorf("limits must be positive")
		}
	}
	for _, v := range []*int64{maxBytesPerChat, maxTotalBytes} {
		if v != nil && *v <= 0 {
			return fmt.Errorf("limits must be positive")
		}
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).
		Scan(&exists); err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	if !exists {
		return domain.ErrNotFound
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_overrides
			(user_id, max_keys_per_user, max_days_limit, max_bytes_per_chat,
			 max_total_bytes_per_user)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET
			max_keys_per_user = EXCLUDED.max_keys_per_user,
			max_days_limit = EXCLUDED.max_days_limit,
			max_bytes_per_chat = EXCLUDED.max_bytes_per_chat,
			max_total_bytes_per_user = EXCLUDED.max_total_bytes_per_user,
			updated_at = now()
	`, userID, maxKeys, maxDays, maxBytesPerChat, maxTotalBytes)
	if err != nil {
		return fmt.Errorf("upsert override: %w", err)
	}
	return nil
}

// DeleteOverride removes all overrides for a user; global defaults apply again.
func (s *AdminService) DeleteOverride(ctx context.Context, userID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM user_overrides WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete override: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// SetAdmin grants or revokes the admin role.
func (s *AdminService) SetAdmin(ctx context.Context, userID int64, admin bool) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE users SET is_admin = $1 WHERE id = $2
	`, admin, userID)
	if err != nil {
		return fmt.Errorf("update admin role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
