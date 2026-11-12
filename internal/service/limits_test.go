package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestLimitsTrimOldestMessagesInChat(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "limits-chat", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key, err := service.NewKeyService(pool).Create(ctx, user.ID, "Limited")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chats SET bytes_limit = 10 WHERE id = $1`, key.ChatID); err != nil {
		t.Fatalf("set chat limit: %v", err)
	}

	now := time.Now().UTC()
	first := insertMessageAt(t, pool, key.ChatID, "aaaa", now.Add(-3*time.Hour))
	insertMessageAt(t, pool, key.ChatID, "bbbb", now.Add(-2*time.Hour))
	insertMessageAt(t, pool, key.ChatID, "cccc", now.Add(-time.Hour))

	svc := service.NewLimitsService(pool)
	results, err := svc.TrimAfterMessage(ctx, key.ChatID)
	if err != nil {
		t.Fatalf("TrimAfterMessage() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v, want one chat", results)
	}
	if results[0].ChatID != key.ChatID || results[0].Deleted != 1 || results[0].ThroughSeq != first {
		t.Errorf("result = %+v, want chat %d deleted 1 through %d", results[0], key.ChatID, first)
	}

	var bodies []string
	rows, err := pool.Query(ctx, `SELECT body FROM messages WHERE chat_id = $1 ORDER BY seq`, key.ChatID)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			t.Fatalf("scan message: %v", err)
		}
		bodies = append(bodies, body)
	}
	rows.Close()
	if len(bodies) != 2 || bodies[0] != "bbbb" || bodies[1] != "cccc" {
		t.Errorf("remaining bodies = %v, want [bbbb cccc]", bodies)
	}

	second, err := svc.TrimAfterMessage(ctx, key.ChatID)
	if err != nil {
		t.Fatalf("second TrimAfterMessage() error = %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second results = %+v, want none", second)
	}
}

func TestLimitsTrimAcrossUserChats(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "limits-user", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	keySvc := service.NewKeyService(pool)
	firstChat, err := keySvc.Create(ctx, user.ID, "First")
	if err != nil {
		t.Fatalf("create first key: %v", err)
	}
	secondChat, err := keySvc.Create(ctx, user.ID, "Second")
	if err != nil {
		t.Fatalf("create second key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE chats SET bytes_limit = 1000 WHERE user_id = $1`, user.ID); err != nil {
		t.Fatalf("set chat limits: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_overrides (user_id, max_total_bytes_per_user) VALUES ($1, 10)
	`, user.ID); err != nil {
		t.Fatalf("set user override: %v", err)
	}

	now := time.Now().UTC()
	firstOld := insertMessageAt(t, pool, firstChat.ChatID, "aaaa", now.Add(-4*time.Hour))
	secondOld := insertMessageAt(t, pool, secondChat.ChatID, "bbbb", now.Add(-3*time.Hour))
	insertMessageAt(t, pool, firstChat.ChatID, "cccc", now.Add(-2*time.Hour))
	insertMessageAt(t, pool, secondChat.ChatID, "dddd", now.Add(-time.Hour))

	results, err := service.NewLimitsService(pool).TrimAfterMessage(ctx, secondChat.ChatID)
	if err != nil {
		t.Fatalf("TrimAfterMessage() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want two chats", results)
	}
	byChat := make(map[int64]domain.TrimResult, len(results))
	for _, result := range results {
		byChat[result.ChatID] = result
	}
	if got := byChat[firstChat.ChatID]; got.Deleted != 1 || got.ThroughSeq != firstOld {
		t.Errorf("first chat result = %+v", got)
	}
	if got := byChat[secondChat.ChatID]; got.Deleted != 1 || got.ThroughSeq != secondOld {
		t.Errorf("second chat result = %+v", got)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages`).Scan(&remaining); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if remaining != 2 {
		t.Errorf("remaining messages = %d, want 2", remaining)
	}
}

func TestLimitsUnknownChat(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()

	_, err := service.NewLimitsService(pool).TrimAfterMessage(context.Background(), 999999)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("TrimAfterMessage() error = %v, want ErrNotFound", err)
	}
}
