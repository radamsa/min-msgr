package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestAdminSettingsUpdate(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()

	admins := service.NewAdminService(pool)
	view, err := admins.Settings(ctx)
	if err != nil {
		t.Fatalf("Settings() error = %v", err)
	}
	if view.MaxKeysPerUser != 2 || view.MaxDaysLimit != 365 {
		t.Errorf("default settings = %+v", view)
	}

	updated, err := admins.UpdateSettings(ctx, 5, 30, 2048, 4096)
	if err != nil {
		t.Fatalf("UpdateSettings() error = %v", err)
	}
	if updated.MaxKeysPerUser != 5 || updated.MaxDaysLimit != 30 ||
		updated.MaxBytesPerChat != 2048 || updated.MaxTotalBytesPerUser != 4096 {
		t.Errorf("updated settings = %+v", updated)
	}

	if _, err := admins.UpdateSettings(ctx, 0, 30, 2048, 4096); err == nil {
		t.Error("UpdateSettings() with zero keys accepted")
	}
}

func TestAdminUsersBlockDelete(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	users := service.NewUserService(pool)

	first, err := users.Create(ctx, "admin-target", "")
	if err != nil {
		t.Fatalf("create first user: %v", err)
	}
	second, err := users.Create(ctx, "admin-other", "")
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	if _, err := service.NewKeyService(pool).Create(ctx, first.ID, "Chat"); err != nil {
		t.Fatalf("create key: %v", err)
	}

	admins := service.NewAdminService(pool)
	items, err := admins.Users(ctx)
	if err != nil {
		t.Fatalf("Users() error = %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("users = %d, want 2", len(items))
	}
	if items[0].ChatCount != 1 || items[1].ChatCount != 0 {
		t.Errorf("chat counts = %d, %d, want 1, 0", items[0].ChatCount, items[1].ChatCount)
	}

	if err := admins.SetBlocked(ctx, first.ID, true); err != nil {
		t.Fatalf("SetBlocked(true) error = %v", err)
	}
	var blocked bool
	var sessions int
	if err := pool.QueryRow(ctx, `SELECT is_blocked FROM users WHERE id = $1`, first.ID).Scan(&blocked); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if !blocked {
		t.Error("user is not blocked")
	}
	// Blocking removes live sessions.
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at)
		VALUES (repeat('aa', 16)::bytea, $1, now() + interval '1 hour')`, first.ID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := admins.SetBlocked(ctx, first.ID, true); err != nil {
		t.Fatalf("second SetBlocked(true) error = %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, first.ID).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions after block = %d, want 0", sessions)
	}
	if err := admins.SetBlocked(ctx, first.ID, false); err != nil {
		t.Fatalf("SetBlocked(false) error = %v", err)
	}

	if err := admins.DeleteUser(ctx, first.ID); err != nil {
		t.Fatalf("DeleteUser() error = %v", err)
	}
	var chats int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chats WHERE user_id = $1`, first.ID).Scan(&chats); err != nil {
		t.Fatalf("count chats: %v", err)
	}
	if chats != 0 {
		t.Errorf("chats after delete = %d, want 0", chats)
	}
	if err := admins.DeleteUser(ctx, first.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second DeleteUser() error = %v, want ErrNotFound", err)
	}
	if err := admins.SetBlocked(ctx, second.ID, true); err != nil {
		t.Fatalf("block second user: %v", err)
	}
}

func TestAdminOverridesLifecycle(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "override-user", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	admins := service.NewAdminService(pool)
	keys := 1
	days := 10
	bytes := int64(2048)
	if err := admins.SetOverride(ctx, user.ID, &keys, &days, &bytes, nil); err != nil {
		t.Fatalf("SetOverride() error = %v", err)
	}

	items, err := admins.Overrides(ctx)
	if err != nil {
		t.Fatalf("Overrides() error = %v", err)
	}
	if len(items) != 1 || items[0].UserID != user.ID {
		t.Fatalf("overrides = %+v, want one for user %d", items, user.ID)
	}
	if items[0].MaxKeysPerUser == nil || *items[0].MaxKeysPerUser != 1 ||
		items[0].MaxDaysLimit == nil || *items[0].MaxDaysLimit != 10 ||
		items[0].MaxBytesPerChat == nil || *items[0].MaxBytesPerChat != 2048 ||
		items[0].MaxTotalBytesPerUser != nil {
		t.Errorf("override values = %+v", items[0])
	}

	// Upsert replaces values; nil fields reset to defaults.
	newKeys := 3
	if err := admins.SetOverride(ctx, user.ID, &newKeys, nil, nil, nil); err != nil {
		t.Fatalf("second SetOverride() error = %v", err)
	}
	items, err = admins.Overrides(ctx)
	if err != nil {
		t.Fatalf("Overrides() after upsert error = %v", err)
	}
	if len(items) != 1 || items[0].MaxKeysPerUser == nil || *items[0].MaxKeysPerUser != 3 {
		t.Fatalf("overrides after upsert = %+v", items)
	}
	if items[0].MaxDaysLimit != nil || items[0].MaxBytesPerChat != nil {
		t.Errorf("nil fields must reset to defaults, got %+v", items[0])
	}

	zero := 0
	if err := admins.SetOverride(ctx, user.ID, &zero, nil, nil, nil); err == nil {
		t.Error("SetOverride() with zero accepted")
	}
	if err := admins.SetOverride(ctx, 999999, nil, nil, nil, nil); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetOverride() unknown user error = %v, want ErrNotFound", err)
	}

	if err := admins.DeleteOverride(ctx, user.ID); err != nil {
		t.Fatalf("DeleteOverride() error = %v", err)
	}
	items, err = admins.Overrides(ctx)
	if err != nil {
		t.Fatalf("Overrides() after delete error = %v", err)
	}
	if len(items) != 0 {
		t.Errorf("overrides after delete = %+v, want none", items)
	}
	if err := admins.DeleteOverride(ctx, user.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("second DeleteOverride() error = %v, want ErrNotFound", err)
	}
}

func TestAdminSetAdmin(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "future-admin", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	admins := service.NewAdminService(pool)
	if err := admins.SetAdmin(ctx, user.ID, true); err != nil {
		t.Fatalf("SetAdmin(true) error = %v", err)
	}
	var isAdmin bool
	if err := pool.QueryRow(ctx, `SELECT is_admin FROM users WHERE id = $1`, user.ID).Scan(&isAdmin); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if !isAdmin {
		t.Error("user is not admin after promote")
	}
	if err := admins.SetAdmin(ctx, user.ID, false); err != nil {
		t.Fatalf("SetAdmin(false) error = %v", err)
	}
	if err := admins.SetAdmin(ctx, 999999, true); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("SetAdmin() unknown user error = %v, want ErrNotFound", err)
	}
}
