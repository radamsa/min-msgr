package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/dav/min-msgr/internal/db"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestMigrateCreatesSchema(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()

	ctx := context.Background()
	tables := []string{
		"users", "oauth_accounts", "sessions", "chats", "api_keys",
		"messages", "admin_settings", "user_overrides", "archives",
	}
	for _, table := range tables {
		var exists bool
		err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = current_schema() AND table_name = $1
			)
		`, table).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s was not created", table)
		}
	}

	var indexDef string
	err := pool.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = current_schema()
			AND tablename = 'messages'
			AND indexdef LIKE '%(chat_id, seq)%'
		LIMIT 1
	`).Scan(&indexDef)
	if err != nil {
		t.Fatalf("find (chat_id, seq) index: %v", err)
	}
	if !strings.Contains(indexDef, "UNIQUE") {
		t.Errorf("(chat_id, seq) index is not unique: %s", indexDef)
	}

	var settingsCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM admin_settings").Scan(&settingsCount); err != nil {
		t.Fatalf("count admin settings: %v", err)
	}
	if settingsCount != 1 {
		t.Errorf("admin_settings rows = %d, want 1", settingsCount)
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()

	if err := db.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
}
