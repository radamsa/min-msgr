package service_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dav/min-msgr/internal/realtime"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/testdb"
)

func insertUser(t *testing.T, pool *pgxpool.Pool, login string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO users (oauth_login, public_id) VALUES ($1, $1 || '#123') RETURNING id
	`, login).Scan(&id)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func insertChat(t *testing.T, pool *pgxpool.Pool, userID int64, label string, days int) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO chats (user_id, label, days_limit, bytes_limit)
		VALUES ($1, $2, $3, 1048576)
		RETURNING id
	`, userID, label, days).Scan(&id)
	if err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	return id
}

func insertMessageAt(t *testing.T, pool *pgxpool.Pool, chatID int64, body string, createdAt time.Time) int64 {
	t.Helper()
	var seq int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO messages (chat_id, author_type, agent_name, body, created_at)
		VALUES ($1, 'agent', 'Agent', $2, $3)
		RETURNING seq
	`, chatID, body, createdAt).Scan(&seq)
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return seq
}

func insertArchive(
	t *testing.T,
	pool *pgxpool.Pool,
	userID, chatID int64,
	path string,
	createdAt, expiresAt time.Time,
) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO archives (user_id, chat_id, label, file_path, size_bytes, created_at, expires_at)
		VALUES ($1, $2, 'archive', $3, 1, $4, $5)
	`, userID, chatID, path, createdAt, expiresAt)
	if err != nil {
		t.Fatalf("insert archive: %v", err)
	}
}

func TestRetentionDeletesMessagesOlderThanDaysLimit(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, pool, "retention")

	oldChatID := insertChat(t, pool, userID, "old", 7)
	newChatID := insertChat(t, pool, userID, "new", 30)

	now := time.Now().UTC()
	insertMessageAt(t, pool, oldChatID, "expired", now.Add(-10*24*time.Hour))
	insertMessageAt(t, pool, oldChatID, "kept", now.Add(-1*24*time.Hour))
	insertMessageAt(t, pool, newChatID, "kept by longer limit", now.Add(-10*24*time.Hour))

	svc := service.NewRetentionService(pool, t.TempDir(), nil)
	report, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if report.MessagesDeleted != 1 {
		t.Errorf("MessagesDeleted = %d, want 1", report.MessagesDeleted)
	}

	var oldChatMessages int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE chat_id = $1`, oldChatID).Scan(&oldChatMessages)
	if err != nil {
		t.Fatalf("count old chat messages: %v", err)
	}
	if oldChatMessages != 1 {
		t.Errorf("old chat messages = %d, want 1", oldChatMessages)
	}
	var body string
	if err := pool.QueryRow(ctx, `SELECT body FROM messages WHERE chat_id = $1`, oldChatID).Scan(&body); err != nil {
		t.Fatalf("read kept message: %v", err)
	}
	if body != "kept" {
		t.Errorf("kept message body = %q, want kept", body)
	}
	var newChatMessages int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE chat_id = $1`, newChatID).Scan(&newChatMessages); err != nil {
		t.Fatalf("count new chat messages: %v", err)
	}
	if newChatMessages != 1 {
		t.Errorf("new chat messages = %d, want 1", newChatMessages)
	}

	second, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if second.MessagesDeleted != 0 {
		t.Errorf("second MessagesDeleted = %d, want 0", second.MessagesDeleted)
	}
}

type retentionRecorder struct {
	events []realtime.Event
}

func (r *retentionRecorder) Publish(chatID int64, event realtime.Event) {
	r.events = append(r.events, event)
}

func TestRetentionPublishesHistoryTrimmed(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, pool, "retention-publish")
	chatID := insertChat(t, pool, userID, "publish", 7)

	now := time.Now().UTC()
	oldSeq := insertMessageAt(t, pool, chatID, "expired", now.Add(-10*24*time.Hour))
	insertMessageAt(t, pool, chatID, "kept", now.Add(-1*24*time.Hour))

	recorder := &retentionRecorder{}
	svc := service.NewRetentionService(pool, t.TempDir(), recorder)
	report, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if report.MessagesDeleted != 1 || report.ChatsTrimmed != 1 {
		t.Errorf("report = %+v, want 1 deleted in 1 chat", report)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("published events = %d, want 1", len(recorder.events))
	}
	event := recorder.events[0]
	if event.Type != "history.trimmed" {
		t.Errorf("event type = %q, want history.trimmed", event.Type)
	}
	trim, ok := event.Payload.(realtime.HistoryTrimmed)
	if !ok {
		t.Fatalf("payload type = %T", event.Payload)
	}
	if trim.ChatID != chatID || trim.Deleted != 1 || trim.ThroughSeq != oldSeq {
		t.Errorf("payload = %+v, want chat %d deleted 1 through %d", trim, chatID, oldSeq)
	}
}

func TestRetentionDeletesExpiredArchives(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, pool, "archives")
	dir := t.TempDir()

	expiredPath := filepath.Join(dir, "expired.zip")
	futurePath := filepath.Join(dir, "future.zip")
	missingPath := filepath.Join(dir, "missing.zip")
	for _, path := range []string{expiredPath, futurePath} {
		if err := os.WriteFile(path, []byte("zip"), 0o600); err != nil {
			t.Fatalf("write archive file: %v", err)
		}
	}

	now := time.Now().UTC()
	insertArchive(t, pool, userID, 1000, expiredPath, now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour))
	insertArchive(t, pool, userID, 1001, missingPath, now.Add(-40*24*time.Hour), now.Add(-1*24*time.Hour))
	insertArchive(t, pool, userID, 1002, futurePath, now.Add(-40*24*time.Hour), now.Add(20*24*time.Hour))

	svc := service.NewRetentionService(pool, dir, nil)
	report, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if report.ArchivesDeleted != 2 || report.ArchivesFailed != 0 {
		t.Errorf("report = %+v, want 2 deleted and 0 failed", report)
	}

	for _, path := range []string{expiredPath, missingPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("archive %s still exists (err = %v)", path, err)
		}
	}
	if _, err := os.Stat(futurePath); err != nil {
		t.Errorf("future archive was removed: %v", err)
	}

	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM archives`).Scan(&remaining); err != nil {
		t.Fatalf("count archives: %v", err)
	}
	if remaining != 1 {
		t.Errorf("archives remaining = %d, want 1", remaining)
	}

	second, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("second RunOnce() error = %v", err)
	}
	if second.ArchivesDeleted != 0 || second.ArchivesFailed != 0 {
		t.Errorf("second report = %+v, want zero", second)
	}
}

func TestRetentionRefusesArchiveOutsideDirectory(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	userID := insertUser(t, pool, "outside")

	outsidePath := filepath.Join(t.TempDir(), "outside.zip")
	if err := os.WriteFile(outsidePath, []byte("zip"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	now := time.Now().UTC()
	insertArchive(t, pool, userID, 2000, outsidePath, now.Add(-40*24*time.Hour), now.Add(-time.Hour))

	svc := service.NewRetentionService(pool, t.TempDir(), nil)
	report, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if report.ArchivesDeleted != 0 || report.ArchivesFailed != 1 {
		t.Errorf("report = %+v, want 0 deleted and 1 failed", report)
	}
	if _, err := os.Stat(outsidePath); err != nil {
		t.Errorf("outside file was removed: %v", err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM archives`).Scan(&remaining); err != nil {
		t.Fatalf("count archives: %v", err)
	}
	if remaining != 1 {
		t.Errorf("archives remaining = %d, want 1", remaining)
	}
}
