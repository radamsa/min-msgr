package service_test

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dav/min-msgr/internal/auth"
	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/store/postgres"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestKeyServiceListAndRotate(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "keys", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	keySvc := service.NewKeyService(pool)

	first, err := keySvc.Create(ctx, user.ID, "First Agent")
	if err != nil {
		t.Fatalf("create first key: %v", err)
	}
	if _, err := keySvc.Create(ctx, user.ID, "Second Agent"); err != nil {
		t.Fatalf("create second key: %v", err)
	}
	insertMessageAt(t, pool, first.ChatID, "hello", time.Now().UTC())

	overview, err := keySvc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(overview.Chats) != 2 {
		t.Fatalf("chats = %d, want 2", len(overview.Chats))
	}
	if overview.MaxKeys != 2 || overview.MaxDaysLimit != 365 || overview.MaxBytesPerChat != 10485760 {
		t.Errorf("limits = %+v", overview)
	}
	var firstChat *service.ChatSummary
	for i := range overview.Chats {
		if overview.Chats[i].ChatID == first.ChatID {
			firstChat = &overview.Chats[i]
		}
	}
	if firstChat == nil {
		t.Fatal("first chat is missing from the list")
	}
	if firstChat.Label != "First Agent" || firstChat.LastAuthor == nil ||
		*firstChat.LastAuthor != "Agent@"+user.Login || firstChat.LastMessageAt == nil {
		t.Errorf("first chat = %+v", firstChat)
	}

	rotated, err := keySvc.Rotate(ctx, user.ID, first.ChatID)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.PlainKey == "" || rotated.PlainKey == first.PlainKey {
		t.Errorf("rotated key = %q, want a new key", rotated.PlainKey)
	}
	if rotated.ChatID != first.ChatID || rotated.Label != first.Label {
		t.Errorf("rotated result = %+v, want chat %d label %q", rotated, first.ChatID, first.Label)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE chat_id = $1`, first.ChatID).
		Scan(&stored); err != nil {
		t.Fatalf("read rotated hash: %v", err)
	}
	if string(stored) != string(auth.Hash(rotated.PlainKey)) {
		t.Error("stored hash does not match the rotated key")
	}

	st := postgres.New(pool)
	if _, err := st.AuthenticateAPIKey(ctx, auth.Hash(first.PlainKey)); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("old key AuthenticateAPIKey() error = %v, want ErrUnauthorized", err)
	}
	if _, err := st.AuthenticateAPIKey(ctx, auth.Hash(rotated.PlainKey)); err != nil {
		t.Errorf("new key AuthenticateAPIKey() error = %v", err)
	}

	other, err := service.NewUserService(pool).Create(ctx, "other", "")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	if _, err := keySvc.Rotate(ctx, other.ID, first.ChatID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign Rotate() error = %v, want ErrNotFound", err)
	}
}

func TestArchiveServiceCreateAndDeleteChat(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "archive", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key, err := service.NewKeyService(pool).Create(ctx, user.ID, "My/Agent")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	insertMessageAt(t, pool, key.ChatID, "first **message**", base)
	if _, err := pool.Exec(ctx, `
		INSERT INTO messages (chat_id, author_type, body, created_at)
		VALUES ($1, 'user', 'user reply', $2)
	`, key.ChatID, base.Add(time.Minute)); err != nil {
		t.Fatalf("insert user message: %v", err)
	}

	dir := t.TempDir()
	archiveSvc := service.NewArchiveService(pool, dir)
	archive, err := archiveSvc.CreateAndDeleteChat(ctx, user.ID, key.ChatID)
	if err != nil {
		t.Fatalf("CreateAndDeleteChat() error = %v", err)
	}
	if archive.ID == 0 || archive.ChatID != key.ChatID || archive.Label != "My/Agent" {
		t.Errorf("archive = %+v", archive)
	}
	if archive.SizeBytes <= 0 || archive.ExpiresAt.Sub(archive.CreatedAt) != service.ArchiveRetention {
		t.Errorf("archive size/expiry = %+v", archive)
	}
	if filepath.Dir(archive.FilePath) != dir {
		t.Errorf("archive path = %q, want directory %q", archive.FilePath, dir)
	}
	if !strings.HasPrefix(filepath.Base(archive.FilePath), "My_Agent_") {
		t.Errorf("archive file name = %q, want sanitized label prefix My_Agent_", archive.FilePath)
	}

	var chats, messages, keys, archives int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM chats`:    &chats,
		`SELECT count(*) FROM messages`: &messages,
		`SELECT count(*) FROM api_keys`: &keys,
		`SELECT count(*) FROM archives`: &archives,
	} {
		if err := pool.QueryRow(ctx, query).Scan(target); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	if chats != 0 || messages != 0 || keys != 0 || archives != 1 {
		t.Errorf("counts = chats %d messages %d keys %d archives %d", chats, messages, keys, archives)
	}

	zipReader, err := zip.OpenReader(archive.FilePath)
	if err != nil {
		t.Fatalf("open archive zip: %v", err)
	}
	defer zipReader.Close()
	if len(zipReader.File) != 1 || !strings.HasSuffix(zipReader.File[0].Name, ".md") {
		t.Fatalf("zip entries = %+v, want one .md file", zipReader.File)
	}
	entry, err := zipReader.File[0].Open()
	if err != nil {
		t.Fatalf("open zip entry: %v", err)
	}
	content, err := io.ReadAll(entry)
	entry.Close()
	if err != nil {
		t.Fatalf("read zip entry: %v", err)
	}
	for _, want := range []string{
		"# My/Agent", user.PublicID, "first **message**", "user reply",
		"Agent@" + user.Login, user.Login, "## ",
	} {
		if !strings.Contains(string(content), want) {
			t.Errorf("archive markdown missing %q:\n%s", want, content)
		}
	}

	list, err := archiveSvc.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != archive.ID {
		t.Errorf("List() = %+v, want archive %d", list, archive.ID)
	}

	opened, file, err := archiveSvc.Open(ctx, user.ID, archive.ID)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if opened.ID != archive.ID {
		t.Errorf("opened archive = %+v", opened)
	}
	file.Close()

	other, err := service.NewUserService(pool).Create(ctx, "other-archive", "")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	if _, _, err := archiveSvc.Open(ctx, other.ID, archive.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign Open() error = %v, want ErrNotFound", err)
	}

	if _, err := archiveSvc.CreateAndDeleteChat(ctx, user.ID, key.ChatID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second CreateAndDeleteChat() error = %v, want ErrNotFound", err)
	}
}

func TestArchiveServiceDeletesFileWhenDatabaseFails(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "archive-fail", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key, err := service.NewKeyService(pool).Create(ctx, user.ID, "Chat")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// A pre-existing archive row with the same chat_id makes the insert fail
	// after the file has been written, so the service must clean the file up.
	dir := t.TempDir()
	if _, err := pool.Exec(ctx, `
		INSERT INTO archives (user_id, chat_id, label, file_path, size_bytes, created_at, expires_at)
		VALUES ($1, $2, 'existing', $3, 1, now() - interval '1 day', now() + interval '1 day')
	`, user.ID, key.ChatID, filepath.Join(dir, "existing.zip")); err != nil {
		t.Fatalf("insert conflicting archive: %v", err)
	}

	archiveSvc := service.NewArchiveService(pool, dir)
	if _, err := archiveSvc.CreateAndDeleteChat(ctx, user.ID, key.ChatID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("CreateAndDeleteChat() error = %v, want ErrConflict", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read archive dir: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("unexpected file left in archive dir: %s", entry.Name())
	}
	var chats int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chats`).Scan(&chats); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if chats != 1 {
		t.Errorf("chats after failed archive = %d, want 1", chats)
	}
}
