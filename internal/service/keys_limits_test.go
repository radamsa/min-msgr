package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestUpdateChatLimitsClampsToAdminMaxima(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "limits-update", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key, err := service.NewKeyService(pool).Create(ctx, user.ID, "Adjustable")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	// Tighten the admin maxima so clamping is observable.
	if _, err := pool.Exec(ctx, `
		UPDATE admin_settings SET max_days_limit = 30, max_bytes_per_chat = 1000
	`); err != nil {
		t.Fatalf("set admin maxima: %v", err)
	}

	svc := service.NewKeyService(pool)
	updated, err := svc.UpdateChatLimits(ctx, user.ID, key.ChatID, 7, 500)
	if err != nil {
		t.Fatalf("UpdateChatLimits() error = %v", err)
	}
	if updated.DaysLimit != 7 || updated.BytesLimit != 500 {
		t.Errorf("updated = %+v, want 7 days 500 bytes", updated)
	}

	// Values above the maxima are clamped, zero keeps the current value.
	updated, err = svc.UpdateChatLimits(ctx, user.ID, key.ChatID, 0, 100000)
	if err != nil {
		t.Fatalf("UpdateChatLimits() clamp error = %v", err)
	}
	if updated.DaysLimit != 7 || updated.BytesLimit != 1000 {
		t.Errorf("clamped = %+v, want 7 days 1000 bytes", updated)
	}

	var days int
	var bytes int64
	if err := pool.QueryRow(ctx, `SELECT days_limit, bytes_limit FROM chats WHERE id = $1`, key.ChatID).
		Scan(&days, &bytes); err != nil {
		t.Fatalf("read chat limits: %v", err)
	}
	if days != 7 || bytes != 1000 {
		t.Errorf("stored limits = %d days %d bytes, want 7 days 1000 bytes", days, bytes)
	}

	// A lower user override replaces the admin maximum.
	if _, err := pool.Exec(ctx, `
		INSERT INTO user_overrides (user_id, max_days_limit) VALUES ($1, 3)
	`, user.ID); err != nil {
		t.Fatalf("set override: %v", err)
	}
	updated, err = svc.UpdateChatLimits(ctx, user.ID, key.ChatID, 10, 0)
	if err != nil {
		t.Fatalf("UpdateChatLimits() override error = %v", err)
	}
	if updated.DaysLimit != 3 || updated.BytesLimit != 1000 {
		t.Errorf("override result = %+v, want 3 days 1000 bytes", updated)
	}
}

func TestUpdateChatLimitsUnknownChat(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "limits-missing", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	_, err = service.NewKeyService(pool).UpdateChatLimits(ctx, user.ID, 999999, 7, 500)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("UpdateChatLimits() error = %v, want ErrNotFound", err)
	}
}

func TestUpdateChatLimitsRequiresOwnership(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	userSvc := service.NewUserService(pool)
	first, err := userSvc.Create(ctx, "limits-owner", "")
	if err != nil {
		t.Fatalf("create first user: %v", err)
	}
	second, err := userSvc.Create(ctx, "limits-stranger", "")
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	key, err := service.NewKeyService(pool).Create(ctx, first.ID, "Foreign")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	_, err = service.NewKeyService(pool).UpdateChatLimits(ctx, second.ID, key.ChatID, 7, 500)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("foreign update error = %v, want ErrNotFound", err)
	}
}

func TestNormalizeChatLimitsKeepsCurrentWhenInvalid(t *testing.T) {
	days, bytes := service.NormalizeChatLimits(-5, -1, 30, 1000, 10, 800)
	if days != 10 || bytes != 800 {
		t.Errorf("NormalizeChatLimits() = %d, %d, want 10, 800", days, bytes)
	}
}
