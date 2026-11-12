package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestOAuthServiceRegistersAndReusesUser(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	svc := service.NewOAuthService(pool)

	first, err := svc.Login(ctx, "github", "42", "alice", "alice@example.com")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if first.ID == 0 || first.Login != "alice" {
		t.Errorf("first user = %+v", first)
	}

	second, err := svc.Login(ctx, "github", "42", "alice-renamed", "other@example.com")
	if err != nil {
		t.Fatalf("second Login() error = %v", err)
	}
	if second.ID != first.ID || second.PublicID != first.PublicID || second.Login != "alice" {
		t.Errorf("second user = %+v, want existing %+v", second, first)
	}

	var email string
	if err := pool.QueryRow(ctx, `SELECT email FROM oauth_accounts WHERE provider = 'github' AND provider_user_id = '42'`).Scan(&email); err != nil {
		t.Fatalf("read oauth account: %v", err)
	}
	if email != "alice@example.com" {
		t.Errorf("oauth email = %q, want alice@example.com", email)
	}

	other, err := svc.Login(ctx, "google", "42", "alice", "")
	if err != nil {
		t.Fatalf("other provider Login() error = %v", err)
	}
	if other.ID == first.ID {
		t.Errorf("different providers reused user id %d", first.ID)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET is_blocked = TRUE WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("block user: %v", err)
	}
	if _, err := svc.Login(ctx, "github", "42", "alice", ""); !errors.Is(err, domain.ErrBlocked) {
		t.Errorf("blocked Login() error = %v, want ErrBlocked", err)
	}
}

func TestOAuthServiceConcurrentFirstLogin(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	svc := service.NewOAuthService(pool)

	const workers = 5
	ids := make([]int64, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user, err := svc.Login(ctx, "github", "race-1", "racer", "")
			ids[i] = user.ID
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d error = %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Errorf("worker %d user id = %d, want %d", i, ids[i], ids[0])
		}
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE oauth_login = 'racer'`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("racer users = %d, want 1", users)
	}
}

func TestSessionServiceLifecycle(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "session", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	svc := service.NewSessionService(pool, time.Hour)

	token, expiresAt, err := svc.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if token == "" || !expiresAt.After(time.Now()) {
		t.Errorf("session token = %q expires %v", token, expiresAt)
	}

	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM sessions`).Scan(&stored); err != nil {
		t.Fatalf("read session hash: %v", err)
	}
	hash := sha256.Sum256([]byte(token))
	if !bytes.Equal(stored, hash[:]) {
		t.Error("stored session hash does not match the token")
	}

	got, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if got.ID != user.ID || got.PublicID != user.PublicID {
		t.Errorf("authenticated user = %+v, want %+v", got, user)
	}

	if err := svc.Delete(ctx, token); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("Authenticate() after delete error = %v, want ErrUnauthorized", err)
	}
}

func TestSessionServiceRejectsExpiredAndBlockedUsers(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "expired", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	svc := service.NewSessionService(pool, time.Hour)

	token, _, err := svc.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE sessions SET created_at = now() - interval '2 hours',
			expires_at = now() - interval '1 hour'
	`); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("expired Authenticate() error = %v, want ErrUnauthorized", err)
	}

	token, _, err = svc.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET is_blocked = TRUE WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("block user: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("blocked Authenticate() error = %v, want ErrUnauthorized", err)
	}
	if _, err := svc.Authenticate(ctx, "unknown-token"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("unknown Authenticate() error = %v, want ErrUnauthorized", err)
	}
}
