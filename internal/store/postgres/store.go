package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/domain"
)

// Store is the PostgreSQL implementation of store.Store.
type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) AuthenticateAPIKey(ctx context.Context, keyHash []byte) (domain.AuthContext, error) {
	var auth domain.AuthContext
	err := s.pool.QueryRow(ctx, `
		SELECT k.chat_id, c.user_id, u.oauth_login
		FROM api_keys k
		JOIN chats c ON c.id = k.chat_id
		JOIN users u ON u.id = c.user_id
		WHERE k.key_hash = $1 AND u.is_blocked = FALSE
	`, keyHash).Scan(&auth.ChatID, &auth.UserID, &auth.Login)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.AuthContext{}, domain.ErrUnauthorized
	}
	if err != nil {
		return domain.AuthContext{}, fmt.Errorf("authenticate api key: %w", err)
	}
	return auth, nil
}

func (s *Store) CreateAgentMessage(
	ctx context.Context,
	chatID int64,
	agentName string,
	body string,
	clientMessageID *string,
) (domain.Message, bool, error) {
	for attempt := 0; attempt < 5; attempt++ {
		message, inserted, err := s.insertAgentMessage(ctx, chatID, agentName, body, clientMessageID)
		if err != nil {
			return domain.Message{}, false, err
		}
		if inserted {
			return message, true, nil
		}
		if clientMessageID == nil {
			return domain.Message{}, false, domain.ErrConflict
		}

		message, err = s.findByClientMessageID(ctx, chatID, *clientMessageID)
		if err == nil {
			return message, false, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Message{}, false, err
		}
	}
	return domain.Message{}, false, domain.ErrConflict
}

func (s *Store) insertAgentMessage(
	ctx context.Context,
	chatID int64,
	agentName string,
	body string,
	clientMessageID *string,
) (domain.Message, bool, error) {
	var message domain.Message
	err := s.pool.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO messages (chat_id, author_type, agent_name, body, client_message_id)
			VALUES ($1, 'agent', $2, $3, $4)
			ON CONFLICT (chat_id, client_message_id)
				WHERE client_message_id IS NOT NULL
			DO NOTHING
			RETURNING seq, chat_id, author_type, agent_name, body, created_at
		)
		SELECT i.seq,
			CASE WHEN i.author_type = 'agent'
				THEN i.agent_name || '@' || u.oauth_login
				ELSE u.oauth_login END,
			i.body,
			i.created_at
		FROM inserted i
		JOIN chats c ON c.id = i.chat_id
		JOIN users u ON u.id = c.user_id
	`, chatID, agentName, body, clientMessageID).Scan(
		&message.Seq, &message.Author, &message.Body, &message.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Message{}, false, nil
	}
	if err != nil {
		if isForeignKeyViolation(err) {
			return domain.Message{}, false, domain.ErrUnauthorized
		}
		return domain.Message{}, false, fmt.Errorf("insert agent message: %w", err)
	}
	return message, true, nil
}

func (s *Store) findByClientMessageID(
	ctx context.Context,
	chatID int64,
	clientMessageID string,
) (domain.Message, error) {
	var message domain.Message
	err := s.pool.QueryRow(ctx, `
		SELECT m.seq,
			CASE WHEN m.author_type = 'agent'
				THEN m.agent_name || '@' || u.oauth_login
				ELSE u.oauth_login END,
			m.body,
			m.created_at
		FROM messages m
		JOIN chats c ON c.id = m.chat_id
		JOIN users u ON u.id = c.user_id
		WHERE m.chat_id = $1 AND m.client_message_id = $2
	`, chatID, clientMessageID).Scan(
		&message.Seq, &message.Author, &message.Body, &message.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Message{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Message{}, fmt.Errorf("find message by client id: %w", err)
	}
	return message, nil
}

// ChatForUser returns a chat only if it belongs to the user.
func (s *Store) ChatForUser(ctx context.Context, userID, chatID int64) (domain.Chat, error) {
	var chat domain.Chat
	err := s.pool.QueryRow(ctx, `
		SELECT id, label, days_limit, bytes_limit
		FROM chats
		WHERE id = $1 AND user_id = $2
	`, chatID, userID).Scan(&chat.ID, &chat.Label, &chat.DaysLimit, &chat.BytesLimit)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Chat{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Chat{}, fmt.Errorf("find chat: %w", err)
	}
	return chat, nil
}

// CreateUserMessage inserts a message written by the user. The chat must belong
// to the user; client_message_id makes retries idempotent.
func (s *Store) CreateUserMessage(
	ctx context.Context,
	userID, chatID int64,
	body string,
	clientMessageID *string,
) (domain.Message, bool, error) {
	if _, err := s.ChatForUser(ctx, userID, chatID); err != nil {
		return domain.Message{}, false, err
	}
	for attempt := 0; attempt < 5; attempt++ {
		message, inserted, err := s.insertUserMessage(ctx, userID, chatID, body, clientMessageID)
		if err != nil {
			return domain.Message{}, false, err
		}
		if inserted {
			return message, true, nil
		}
		if clientMessageID == nil {
			return domain.Message{}, false, domain.ErrNotFound
		}
		message, err = s.findByClientMessageID(ctx, chatID, *clientMessageID)
		if err == nil {
			return message, false, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return domain.Message{}, false, err
		}
	}
	return domain.Message{}, false, domain.ErrConflict
}

func (s *Store) insertUserMessage(
	ctx context.Context,
	userID, chatID int64,
	body string,
	clientMessageID *string,
) (domain.Message, bool, error) {
	var message domain.Message
	err := s.pool.QueryRow(ctx, `
		WITH target AS (
			SELECT id FROM chats WHERE id = $1 AND user_id = $2
		), inserted AS (
			INSERT INTO messages (chat_id, author_type, body, client_message_id)
			SELECT target.id, 'user', $3, $4 FROM target
			ON CONFLICT (chat_id, client_message_id)
				WHERE client_message_id IS NOT NULL
			DO NOTHING
			RETURNING seq, chat_id, body, created_at
		)
		SELECT i.seq, u.oauth_login, i.body, i.created_at
		FROM inserted i
		JOIN chats c ON c.id = i.chat_id
		JOIN users u ON u.id = c.user_id
	`, chatID, userID, body, clientMessageID).Scan(
		&message.Seq, &message.Author, &message.Body, &message.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Message{}, false, nil
	}
	if err != nil {
		return domain.Message{}, false, fmt.Errorf("insert user message: %w", err)
	}
	return message, true, nil
}

func (s *Store) ListMessages(
	ctx context.Context,
	chatID int64,
	params domain.ListParams,
) (domain.MessagePage, error) {
	return s.listMessages(ctx, chatID, 0, params)
}

// ListMessagesForUser lists a chat's messages after checking that the chat
// belongs to the user. An unknown or foreign chat yields an empty page.
func (s *Store) ListMessagesForUser(
	ctx context.Context,
	userID, chatID int64,
	params domain.ListParams,
) (domain.MessagePage, error) {
	return s.listMessages(ctx, chatID, userID, params)
}

func (s *Store) listMessages(
	ctx context.Context,
	chatID, userID int64,
	params domain.ListParams,
) (domain.MessagePage, error) {
	var query strings.Builder
	query.WriteString(`
		SELECT m.seq,
			CASE WHEN m.author_type = 'agent'
				THEN m.agent_name || '@' || u.oauth_login
				ELSE u.oauth_login END,
			m.body,
			m.created_at
		FROM messages m
		JOIN chats c ON c.id = m.chat_id
		JOIN users u ON u.id = c.user_id
		WHERE m.chat_id = $1
	`)
	args := []any{chatID}
	if userID != 0 {
		args = append(args, userID)
		fmt.Fprintf(&query, " AND c.user_id = $%d", len(args))
	}

	if params.Since != nil {
		args = append(args, *params.Since)
		fmt.Fprintf(&query, " AND m.created_at >= $%d", len(args))
	}
	if params.AfterSeq != nil {
		args = append(args, *params.AfterSeq)
		fmt.Fprintf(&query, " AND m.seq > $%d", len(args))
	}
	if params.BeforeSeq != nil {
		args = append(args, *params.BeforeSeq)
		fmt.Fprintf(&query, " AND m.seq < $%d", len(args))
	}

	ascending := params.Since != nil
	if ascending {
		query.WriteString(" ORDER BY m.seq ASC")
	} else {
		query.WriteString(" ORDER BY m.seq DESC")
	}
	args = append(args, params.Limit+1)
	fmt.Fprintf(&query, " LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, query.String(), args...)
	if err != nil {
		return domain.MessagePage{}, fmt.Errorf("list messages: %w", err)
	}
	defer rows.Close()

	messages := make([]domain.Message, 0, params.Limit)
	for rows.Next() {
		var message domain.Message
		if err := rows.Scan(
			&message.Seq, &message.Author, &message.Body, &message.CreatedAt,
		); err != nil {
			return domain.MessagePage{}, fmt.Errorf("scan message: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return domain.MessagePage{}, fmt.Errorf("iterate messages: %w", err)
	}

	hasMore := len(messages) > params.Limit
	if hasMore {
		messages = messages[:params.Limit]
	}
	if !ascending {
		reverse(messages)
	}
	return domain.MessagePage{Messages: messages, HasMore: hasMore}, nil
}

func reverse(messages []domain.Message) {
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
