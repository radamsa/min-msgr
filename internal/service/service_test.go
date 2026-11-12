package service_test

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/dav/min-msgr/internal/auth"
	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestUserServiceCreate(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	svc := service.NewUserService(pool)
	ctx := context.Background()

	first, err := svc.Create(ctx, "alice", "alice@example.com")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	second, err := svc.Create(ctx, "alice", "")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	pattern := regexp.MustCompile(`^alice#[0-9]{3}$`)
	if !pattern.MatchString(first.PublicID) || !pattern.MatchString(second.PublicID) {
		t.Errorf("public ids = %q, %q", first.PublicID, second.PublicID)
	}
	if first.PublicID == second.PublicID {
		t.Errorf("public ids are not unique: %q", first.PublicID)
	}
	if first.ID == 0 || second.ID == 0 {
		t.Errorf("ids were not assigned: %d, %d", first.ID, second.ID)
	}

	if _, err := svc.Create(ctx, "   ", ""); err == nil {
		t.Fatal("Create() with empty login error = nil, want error")
	}
}

func TestKeyServiceCreate(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "bob", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	svc := service.NewKeyService(pool)

	first, err := svc.Create(ctx, user.ID, "My Agent")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if first.ChatID == 0 || first.PlainKey == "" {
		t.Fatalf("result = %+v", first)
	}
	if !bytes.HasPrefix([]byte(first.PlainKey), []byte(auth.KeyPrefix)) {
		t.Errorf("key %q does not have prefix %q", first.PlainKey, auth.KeyPrefix)
	}

	var label string
	var daysLimit int
	var bytesLimit int64
	if err := pool.QueryRow(ctx, `
		SELECT label, days_limit, bytes_limit FROM chats WHERE id = $1
	`, first.ChatID).Scan(&label, &daysLimit, &bytesLimit); err != nil {
		t.Fatalf("read chat: %v", err)
	}
	if label != "My Agent" || daysLimit != 365 || bytesLimit != 10485760 {
		t.Errorf("chat = label %q days %d bytes %d", label, daysLimit, bytesLimit)
	}

	var storedHash []byte
	if err := pool.QueryRow(ctx, `
		SELECT key_hash FROM api_keys WHERE chat_id = $1
	`, first.ChatID).Scan(&storedHash); err != nil {
		t.Fatalf("read api key: %v", err)
	}
	if !bytes.Equal(storedHash, auth.Hash(first.PlainKey)) {
		t.Error("stored key hash does not match plaintext key")
	}

	if _, err := svc.Create(ctx, user.ID, "Second"); err != nil {
		t.Fatalf("second Create() error = %v", err)
	}
	if _, err := svc.Create(ctx, user.ID, "Third"); !errors.Is(err, domain.ErrKeyLimit) {
		t.Errorf("third Create() error = %v, want ErrKeyLimit", err)
	}
	if _, err := svc.Create(ctx, user.ID, "   "); err == nil {
		t.Error("Create() with empty label error = nil, want error")
	}
	if _, err := svc.Create(ctx, 999999, "Orphan"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("Create() for missing user error = %v, want ErrNotFound", err)
	}
}

func TestKeyServiceUsesUserOverrides(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "carol", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_overrides (
			user_id, max_keys_per_user, max_days_limit, max_bytes_per_chat
		) VALUES ($1, 1, 7, 1000)
	`, user.ID); err != nil {
		t.Fatalf("insert override: %v", err)
	}

	svc := service.NewKeyService(pool)
	result, err := svc.Create(ctx, user.ID, "Overridden")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	var daysLimit int
	var bytesLimit int64
	if err := pool.QueryRow(ctx, `
		SELECT days_limit, bytes_limit FROM chats WHERE id = $1
	`, result.ChatID).Scan(&daysLimit, &bytesLimit); err != nil {
		t.Fatalf("read chat: %v", err)
	}
	if daysLimit != 7 || bytesLimit != 1000 {
		t.Errorf("limits = days %d bytes %d, want 7 and 1000", daysLimit, bytesLimit)
	}
	if _, err := svc.Create(ctx, user.ID, "Blocked"); !errors.Is(err, domain.ErrKeyLimit) {
		t.Errorf("second Create() error = %v, want ErrKeyLimit", err)
	}
}
