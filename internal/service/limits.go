package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
)

// LimitsService enforces byte limits after messages are inserted. It deletes
// the oldest messages in the chat first, then the user's oldest messages
// globally if the per-user limit is still exceeded.
type LimitsService struct {
	pool *pgxpool.Pool
}

func NewLimitsService(pool *pgxpool.Pool) *LimitsService {
	return &LimitsService{pool: pool}
}

// TrimAfterMessage enforces limits for the chat's owner. The returned results
// cover every chat that lost messages, including chats other than chatID when
// the per-user limit applies.
func (s *LimitsService) TrimAfterMessage(ctx context.Context, chatID int64) ([]domain.TrimResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin trim: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	var userID, chatBytesLimit, userBytesLimit int64
	err = tx.QueryRow(ctx, `
		SELECT c.user_id, c.bytes_limit,
			COALESCE(uo.max_total_bytes_per_user, a.max_total_bytes_per_user)
		FROM chats c
		JOIN admin_settings a ON a.id = TRUE
		LEFT JOIN user_overrides uo ON uo.user_id = c.user_id
		WHERE c.id = $1
	`, chatID).Scan(&userID, &chatBytesLimit, &userBytesLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read trim limits: %w", err)
	}

	// Serialize trimming for one user so concurrent sends cannot overshoot the
	// limits or delete the same rows twice.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		fmt.Sprintf("minmsgr-trim:%d", userID),
	); err != nil {
		return nil, fmt.Errorf("lock trim: %w", err)
	}

	results := make(map[int64]*domain.TrimResult)
	if err := trimChat(ctx, tx, chatID, chatBytesLimit, results); err != nil {
		return nil, err
	}
	if err := trimUser(ctx, tx, userID, userBytesLimit, results); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit trim: %w", err)
	}

	ordered := make([]domain.TrimResult, 0, len(results))
	for _, result := range results {
		ordered = append(ordered, *result)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ChatID < ordered[j].ChatID })
	return ordered, nil
}

func trimChat(
	ctx context.Context,
	tx pgx.Tx,
	chatID, bytesLimit int64,
	results map[int64]*domain.TrimResult,
) error {
	rows, err := tx.Query(ctx, `
		WITH ordered AS (
			SELECT seq, sum(bytes) OVER (ORDER BY seq DESC) AS running
			FROM messages
			WHERE chat_id = $1
		)
		DELETE FROM messages
		WHERE seq IN (SELECT seq FROM ordered WHERE running > $2)
		RETURNING chat_id, seq
	`, chatID, bytesLimit)
	if err != nil {
		return fmt.Errorf("trim chat messages: %w", err)
	}
	defer rows.Close()
	if err := collectTrimmed(rows, results); err != nil {
		return err
	}
	return nil
}

func trimUser(
	ctx context.Context,
	tx pgx.Tx,
	userID, bytesLimit int64,
	results map[int64]*domain.TrimResult,
) error {
	rows, err := tx.Query(ctx, `
		WITH ordered AS (
			SELECT m.seq, m.chat_id,
				sum(m.bytes) OVER (ORDER BY m.created_at DESC, m.seq DESC) AS running
			FROM messages m
			JOIN chats c ON c.id = m.chat_id
			WHERE c.user_id = $1
		)
		DELETE FROM messages
		WHERE seq IN (SELECT seq FROM ordered WHERE running > $2)
		RETURNING chat_id, seq
	`, userID, bytesLimit)
	if err != nil {
		return fmt.Errorf("trim user messages: %w", err)
	}
	defer rows.Close()
	if err := collectTrimmed(rows, results); err != nil {
		return err
	}
	return nil
}

func collectTrimmed(rows pgx.Rows, results map[int64]*domain.TrimResult) error {
	for rows.Next() {
		var chatID, seq int64
		if err := rows.Scan(&chatID, &seq); err != nil {
			return fmt.Errorf("scan trimmed message: %w", err)
		}
		result := results[chatID]
		if result == nil {
			result = &domain.TrimResult{ChatID: chatID}
			results[chatID] = result
		}
		result.Deleted++
		if seq > result.ThroughSeq {
			result.ThroughSeq = seq
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate trimmed messages: %w", err)
	}
	return nil
}
