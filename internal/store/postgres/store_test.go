package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/auth"
	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/store/postgres"
	"github.com/dav/min-msgr/internal/testdb"
)

type fixture struct {
	userID int64
	chatID int64
	login  string
}

func seedChat(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	login := fmt.Sprintf("user%d", time.Now().UnixNano())

	var userID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO users (oauth_login, public_id)
		VALUES ($1, $1 || '#123')
		RETURNING id
	`, login).Scan(&userID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var chatID int64
	err = pool.QueryRow(ctx, `
		INSERT INTO chats (user_id, label, days_limit, bytes_limit)
		VALUES ($1, 'test chat', 30, 1048576)
		RETURNING id
	`, userID).Scan(&chatID)
	if err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	return fixture{userID: userID, chatID: chatID, login: login}
}

func insertKey(t *testing.T, pool *pgxpool.Pool, chatID int64) string {
	t.Helper()
	plain, hash, err := auth.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	_, err = pool.Exec(context.Background(), `
		INSERT INTO api_keys (chat_id, label, key_hash) VALUES ($1, 'test key', $2)
	`, chatID, hash)
	if err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return plain
}

func TestAuthenticateAPIKey(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	fx := seedChat(t, pool)
	plain := insertKey(t, pool, fx.chatID)
	st := postgres.New(pool)

	authContext, err := st.AuthenticateAPIKey(context.Background(), auth.Hash(plain))
	if err != nil {
		t.Fatalf("AuthenticateAPIKey() error = %v", err)
	}
	if authContext.ChatID != fx.chatID || authContext.UserID != fx.userID || authContext.Login != fx.login {
		t.Errorf("auth context = %+v, want chat %d user %d login %q", authContext, fx.chatID, fx.userID, fx.login)
	}

	_, err = st.AuthenticateAPIKey(context.Background(), auth.Hash("sk_wrong"))
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("wrong key error = %v, want ErrUnauthorized", err)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE users SET is_blocked = TRUE WHERE id = $1`, fx.userID); err != nil {
		t.Fatalf("block user: %v", err)
	}
	_, err = st.AuthenticateAPIKey(context.Background(), auth.Hash(plain))
	if !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("blocked user error = %v, want ErrUnauthorized", err)
	}
}

func TestCreateAgentMessage(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	fx := seedChat(t, pool)
	st := postgres.New(pool)
	ctx := context.Background()

	first, created, err := st.CreateAgentMessage(ctx, fx.chatID, "MyAgent", "hello", nil)
	if err != nil {
		t.Fatalf("CreateAgentMessage() error = %v", err)
	}
	if !created {
		t.Fatal("first message was not created")
	}
	if first.Seq <= 0 || first.Author != "MyAgent@"+fx.login || first.Body != "hello" {
		t.Errorf("first message = %+v", first)
	}

	clientID := "client-1"
	second, created, err := st.CreateAgentMessage(ctx, fx.chatID, "MyAgent", "hello", &clientID)
	if err != nil {
		t.Fatalf("CreateAgentMessage() error = %v", err)
	}
	if !created {
		t.Fatal("second message was not created")
	}

	replayed, created, err := st.CreateAgentMessage(ctx, fx.chatID, "MyAgent", "different body", &clientID)
	if err != nil {
		t.Fatalf("idempotent CreateAgentMessage() error = %v", err)
	}
	if created {
		t.Fatal("idempotent message was created again")
	}
	if replayed.Seq != second.Seq || replayed.Body != "hello" {
		t.Errorf("replayed message = %+v, want seq %d body hello", replayed, second.Seq)
	}

	otherID := "client-2"
	third, created, err := st.CreateAgentMessage(ctx, fx.chatID, "MyAgent", "new", &otherID)
	if err != nil {
		t.Fatalf("CreateAgentMessage() error = %v", err)
	}
	if !created || third.Seq <= replayed.Seq {
		t.Errorf("third message = %+v created=%v, want new seq after %d", third, created, replayed.Seq)
	}

	withoutID1, created1, err := st.CreateAgentMessage(ctx, fx.chatID, "MyAgent", "no id", nil)
	if err != nil {
		t.Fatalf("CreateAgentMessage() error = %v", err)
	}
	withoutID2, created2, err := st.CreateAgentMessage(ctx, fx.chatID, "MyAgent", "no id", nil)
	if err != nil {
		t.Fatalf("CreateAgentMessage() error = %v", err)
	}
	if !created1 || !created2 || withoutID2.Seq <= withoutID1.Seq {
		t.Errorf("messages without client id were not both created")
	}
}

func TestListMessages(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	fx := seedChat(t, pool)
	st := postgres.New(pool)
	ctx := context.Background()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		message, _, err := st.CreateAgentMessage(ctx, fx.chatID, "MyAgent", fmt.Sprintf("message %d", i), nil)
		if err != nil {
			t.Fatalf("create message %d: %v", i, err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE messages SET created_at = $1 WHERE seq = $2`,
			base.Add(time.Duration(i)*time.Minute), message.Seq,
		); err != nil {
			t.Fatalf("set created_at for message %d: %v", i, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO messages (chat_id, author_type, body, created_at)
		VALUES ($1, 'user', 'from user', $2)
	`, fx.chatID, base.Add(6*time.Minute)); err != nil {
		t.Fatalf("insert user message: %v", err)
	}

	page, err := st.ListMessages(ctx, fx.chatID, domain.ListParams{Limit: 2})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Seq != 5 || page.Messages[1].Seq != 6 || !page.HasMore {
		t.Errorf("latest page = %+v", page)
	}
	if page.Messages[1].Author != fx.login {
		t.Errorf("user message author = %q, want %q", page.Messages[1].Author, fx.login)
	}

	before := int64(5)
	page, err = st.ListMessages(ctx, fx.chatID, domain.ListParams{Limit: 2, BeforeSeq: &before})
	if err != nil {
		t.Fatalf("ListMessages(before) error = %v", err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Seq != 3 || page.Messages[1].Seq != 4 || !page.HasMore {
		t.Errorf("before page = %+v", page)
	}

	before = 3
	page, err = st.ListMessages(ctx, fx.chatID, domain.ListParams{Limit: 2, BeforeSeq: &before})
	if err != nil {
		t.Fatalf("ListMessages(before) error = %v", err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Seq != 1 || page.Messages[1].Seq != 2 || page.HasMore {
		t.Errorf("oldest page = %+v", page)
	}

	since := base.Add(3 * time.Minute)
	page, err = st.ListMessages(ctx, fx.chatID, domain.ListParams{Limit: 2, Since: &since})
	if err != nil {
		t.Fatalf("ListMessages(since) error = %v", err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Seq != 3 || page.Messages[1].Seq != 4 || !page.HasMore {
		t.Errorf("since page = %+v", page)
	}

	after := int64(2)
	page, err = st.ListMessages(ctx, fx.chatID, domain.ListParams{Limit: 2, Since: &since, AfterSeq: &after})
	if err != nil {
		t.Fatalf("ListMessages(after) error = %v", err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Seq != 3 || page.Messages[1].Seq != 4 || !page.HasMore {
		t.Errorf("after page = %+v", page)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM messages WHERE seq = 3`); err != nil {
		t.Fatalf("delete message: %v", err)
	}
	after = 2
	page, err = st.ListMessages(ctx, fx.chatID, domain.ListParams{Limit: 2, Since: &since, AfterSeq: &after})
	if err != nil {
		t.Fatalf("ListMessages(after deleted cursor) error = %v", err)
	}
	if len(page.Messages) == 0 || page.Messages[0].Seq != 4 {
		t.Errorf("deleted cursor page = %+v, want first seq 4", page)
	}
}

func TestListMessagesEmptyChat(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	fx := seedChat(t, pool)
	st := postgres.New(pool)

	page, err := st.ListMessages(context.Background(), fx.chatID, domain.ListParams{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages() error = %v", err)
	}
	if len(page.Messages) != 0 || page.HasMore {
		t.Errorf("empty page = %+v", page)
	}
}
