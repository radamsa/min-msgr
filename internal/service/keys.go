package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/auth"
	"github.com/dav/min-msgr/internal/domain"
)

// CreateKeyResult contains the only copy of the plaintext API key.
type CreateKeyResult struct {
	ChatID   int64
	Label    string
	PlainKey string
}

// ChatSummary is one chat as shown in the chat and key lists.
type ChatSummary struct {
	ChatID        int64
	Label         string
	CreatedAt     time.Time
	DaysLimit     int
	BytesLimit    int64
	LastAuthor    *string
	LastMessageAt *time.Time
}

// KeyOverview contains the user's chats and effective limits.
type KeyOverview struct {
	Chats           []ChatSummary
	MaxKeys         int
	MaxDaysLimit    int
	MaxBytesPerChat int64
	// MaxTotalBytesPerUser is the user's total history budget; users can view
	// it but only admins change it.
	MaxTotalBytesPerUser int64
	// UsedBytesPerUser is the current size of the user's total history.
	UsedBytesPerUser int64
}

type KeyService struct {
	pool *pgxpool.Pool
}

func NewKeyService(pool *pgxpool.Pool) *KeyService {
	return &KeyService{pool: pool}
}

// Create atomically creates a chat and its API key. The key label is also the
// chat name and cannot be changed later.
func (s *KeyService) Create(ctx context.Context, userID int64, label string) (CreateKeyResult, error) {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > 100 {
		return CreateKeyResult{}, fmt.Errorf("label must be 1-100 characters")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return CreateKeyResult{}, fmt.Errorf("begin create key: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// Lock the user row so concurrent key creation cannot exceed the limit.
	var blocked bool
	err = tx.QueryRow(ctx, `
		SELECT is_blocked FROM users WHERE id = $1 FOR UPDATE
	`, userID).Scan(&blocked)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreateKeyResult{}, domain.ErrNotFound
	}
	if err != nil {
		return CreateKeyResult{}, fmt.Errorf("lock user: %w", err)
	}
	if blocked {
		return CreateKeyResult{}, domain.ErrUnauthorized
	}

	var maxKeys int
	var daysLimit int
	var bytesLimit int64
	err = tx.QueryRow(ctx, `
		SELECT COALESCE(uo.max_keys_per_user, a.max_keys_per_user),
			COALESCE(uo.max_days_limit, a.max_days_limit),
			COALESCE(uo.max_bytes_per_chat, a.max_bytes_per_chat)
		FROM admin_settings a
		LEFT JOIN user_overrides uo ON uo.user_id = $1
		WHERE a.id = TRUE
	`, userID).Scan(&maxKeys, &daysLimit, &bytesLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreateKeyResult{}, fmt.Errorf("admin settings are missing")
	}
	if err != nil {
		return CreateKeyResult{}, fmt.Errorf("read limits: %w", err)
	}

	var currentKeys int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM chats WHERE user_id = $1`, userID).Scan(&currentKeys); err != nil {
		return CreateKeyResult{}, fmt.Errorf("count keys: %w", err)
	}
	if currentKeys >= maxKeys {
		return CreateKeyResult{}, domain.ErrKeyLimit
	}

	var chatID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO chats (user_id, label, days_limit, bytes_limit)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, userID, label, daysLimit, bytesLimit).Scan(&chatID)
	if err != nil {
		if isForeignKeyViolation(err) {
			return CreateKeyResult{}, domain.ErrNotFound
		}
		return CreateKeyResult{}, fmt.Errorf("create chat: %w", err)
	}

	plain, hash, err := auth.Generate()
	if err != nil {
		return CreateKeyResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO api_keys (chat_id, label, key_hash)
		VALUES ($1, $2, $3)
	`, chatID, label, hash); err != nil {
		return CreateKeyResult{}, fmt.Errorf("create api key: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateKeyResult{}, fmt.Errorf("commit create key: %w", err)
	}
	return CreateKeyResult{ChatID: chatID, Label: label, PlainKey: plain}, nil
}

// UpdateChatLimits sets the user's per-chat days and bytes limits, clamped to
// the effective admin maxima. It returns the applied values.
func (s *KeyService) UpdateChatLimits(
	ctx context.Context,
	userID, chatID int64,
	daysLimit int,
	bytesLimit int64,
) (domain.Chat, error) {
	var effective struct {
		maxDays     int
		maxBytes    int64
		currentDays int
		currentByte int64
	}
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(uo.max_days_limit, a.max_days_limit),
			COALESCE(uo.max_bytes_per_chat, a.max_bytes_per_chat),
			c.days_limit, c.bytes_limit
		FROM admin_settings a
		JOIN chats c ON c.id = $1 AND c.user_id = $2
		LEFT JOIN user_overrides uo ON uo.user_id = $2
		WHERE a.id = TRUE
	`, chatID, userID).Scan(
		&effective.maxDays, &effective.maxBytes,
		&effective.currentDays, &effective.currentByte,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Chat{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Chat{}, fmt.Errorf("read chat limits: %w", err)
	}

	daysLimit, bytesLimit = NormalizeChatLimits(
		daysLimit, bytesLimit, effective.maxDays, effective.maxBytes,
		effective.currentDays, effective.currentByte,
	)

	tag, err := s.pool.Exec(ctx, `
		UPDATE chats SET days_limit = $1, bytes_limit = $2
		WHERE id = $3 AND user_id = $4
	`, daysLimit, bytesLimit, chatID, userID)
	if err != nil {
		return domain.Chat{}, fmt.Errorf("update chat limits: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Chat{}, domain.ErrNotFound
	}
	return domain.Chat{ID: chatID, DaysLimit: daysLimit, BytesLimit: bytesLimit}, nil
}

// NormalizeChatLimits clamps user-provided limits to the admin maxima. Zero
// keeps the current value; invalid values (empty/negative) fall back to the
// current value too.
func NormalizeChatLimits(
	daysLimit int,
	bytesLimit int64,
	maxDays int,
	maxBytes int64,
	currentDays int,
	currentBytes int64,
) (int, int64) {
	if daysLimit <= 0 {
		daysLimit = currentDays
	}
	if daysLimit > maxDays {
		daysLimit = maxDays
	}
	if bytesLimit <= 0 {
		bytesLimit = currentBytes
	}
	if bytesLimit > maxBytes {
		bytesLimit = maxBytes
	}
	return daysLimit, bytesLimit
}

// List returns the user's chats with their latest message and effective limits.
func (s *KeyService) List(ctx context.Context, userID int64) (KeyOverview, error) {
	var overview KeyOverview
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(uo.max_keys_per_user, a.max_keys_per_user),
			COALESCE(uo.max_days_limit, a.max_days_limit),
			COALESCE(uo.max_bytes_per_chat, a.max_bytes_per_chat),
			COALESCE(uo.max_total_bytes_per_user, a.max_total_bytes_per_user),
			(SELECT COALESCE(sum(m.bytes), 0) FROM messages m
				JOIN chats c ON c.id = m.chat_id
				WHERE c.user_id = $1)
		FROM admin_settings a
		LEFT JOIN user_overrides uo ON uo.user_id = $1
		WHERE a.id = TRUE
	`, userID).Scan(
		&overview.MaxKeys, &overview.MaxDaysLimit, &overview.MaxBytesPerChat,
		&overview.MaxTotalBytesPerUser, &overview.UsedBytesPerUser,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return KeyOverview{}, fmt.Errorf("admin settings are missing")
	}
	if err != nil {
		return KeyOverview{}, fmt.Errorf("read limits: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.label, c.created_at, c.days_limit, c.bytes_limit,
			last.author, last.created_at
		FROM chats c
		JOIN users u ON u.id = c.user_id
		LEFT JOIN LATERAL (
			SELECT CASE WHEN m.author_type = 'agent'
					THEN m.agent_name || '@' || u.oauth_login
					ELSE u.oauth_login END AS author,
				m.created_at
			FROM messages m
			WHERE m.chat_id = c.id
			ORDER BY m.seq DESC
			LIMIT 1
		) last ON TRUE
		WHERE c.user_id = $1
		ORDER BY c.created_at DESC, c.id DESC
	`, userID)
	if err != nil {
		return KeyOverview{}, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()

	overview.Chats = make([]ChatSummary, 0)
	for rows.Next() {
		var chat ChatSummary
		if err := rows.Scan(
			&chat.ChatID, &chat.Label, &chat.CreatedAt, &chat.DaysLimit, &chat.BytesLimit,
			&chat.LastAuthor, &chat.LastMessageAt,
		); err != nil {
			return KeyOverview{}, fmt.Errorf("scan chat: %w", err)
		}
		overview.Chats = append(overview.Chats, chat)
	}
	if err := rows.Err(); err != nil {
		return KeyOverview{}, fmt.Errorf("iterate chats: %w", err)
	}
	return overview, nil
}

// Rotate replaces the chat's API key in place and returns the new plaintext
// key. The old key stops working immediately.
func (s *KeyService) Rotate(ctx context.Context, userID, chatID int64) (CreateKeyResult, error) {
	plain, hash, err := auth.Generate()
	if err != nil {
		return CreateKeyResult{}, err
	}
	var result CreateKeyResult
	err = s.pool.QueryRow(ctx, `
		UPDATE api_keys k
		SET key_hash = $1
		FROM chats c
		WHERE k.chat_id = c.id AND c.id = $2 AND c.user_id = $3
		RETURNING k.chat_id, c.label
	`, hash, chatID, userID).Scan(&result.ChatID, &result.Label)
	if errors.Is(err, pgx.ErrNoRows) {
		return CreateKeyResult{}, domain.ErrNotFound
	}
	if err != nil {
		return CreateKeyResult{}, fmt.Errorf("rotate api key: %w", err)
	}
	result.PlainKey = plain
	return result, nil
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
