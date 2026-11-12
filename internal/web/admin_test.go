package web_test

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/store/postgres"
	"github.com/dav/min-msgr/internal/testdb"
	"github.com/dav/min-msgr/internal/web"
)

// TestAdminPanelFlow exercises the admin panel end to end: access control,
// global settings, user blocking, and overrides.
func TestAdminPanelFlow(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()

	userSvc := service.NewUserService(pool)
	admin, err := userSvc.Create(ctx, "panel-admin", "")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	plain, err := userSvc.Create(ctx, "panel-user", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	admins := service.NewAdminService(pool)
	if err := admins.SetAdmin(ctx, admin.ID, true); err != nil {
		t.Fatalf("promote admin: %v", err)
	}

	sessionSvc := service.NewSessionService(pool, time.Hour)
	adminToken, _, err := sessionSvc.Create(ctx, admin.ID)
	if err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	userToken, _, err := sessionSvc.Create(ctx, plain.ID)
	if err != nil {
		t.Fatalf("create user session: %v", err)
	}

	handler, err := web.NewHandler(web.Options{
		OAuth:    service.NewOAuthService(pool),
		Sessions: sessionSvc,
		Keys:     service.NewKeyService(pool),
		Archives: service.NewArchiveService(pool, t.TempDir()),
		Chats:    postgres.New(pool),
		Admins:   admins,
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server, client := newTestServer(t, handler.Routes())
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}

	// Anonymous and non-admin users cannot reach the panel.
	resp, err := client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin anonymous: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous GET /admin = %d Location %q, want 303 /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}

	client.Jar.SetCookies(serverURL, []*http.Cookie{{Name: "minmsgr_session", Value: userToken, Path: "/"}})
	resp, err = client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin as user: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("user GET /admin = %d, want 403", resp.StatusCode)
	}
	if !strings.Contains(body, "только для администраторов") {
		t.Errorf("forbidden page = %s", body)
	}

	client.Jar.SetCookies(serverURL, []*http.Cookie{{Name: "minmsgr_session", Value: adminToken, Path: "/"}})
	resp, err = client.Get(server.URL + "/admin")
	if err != nil {
		t.Fatalf("GET /admin as admin: %v", err)
	}
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin GET /admin = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"Админ-панель", "Глобальные лимиты", "panel-admin", "panel-user"} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page missing %q: %s", want, body)
		}
	}

	// Update global settings.
	resp, err = client.PostForm(server.URL+"/admin/settings", url.Values{
		"max_keys_per_user":        {"5"},
		"max_days_limit":           {"30"},
		"max_bytes_per_chat":       {"2048"},
		"max_total_bytes_per_user": {"8192"},
	})
	if err != nil {
		t.Fatalf("POST /admin/settings: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings update = %d, want 303", resp.StatusCode)
	}
	var maxKeys, maxDays int
	var maxBytes, maxTotal int64
	if err := pool.QueryRow(ctx, `
		SELECT max_keys_per_user, max_days_limit, max_bytes_per_chat,
			max_total_bytes_per_user FROM admin_settings
	`).Scan(&maxKeys, &maxDays, &maxBytes, &maxTotal); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if maxKeys != 5 || maxDays != 30 || maxBytes != 2048 || maxTotal != 8192 {
		t.Errorf("settings = %d %d %d %d, want 5 30 2048 8192", maxKeys, maxDays, maxBytes, maxTotal)
	}

	// Block the user; their session must die.
	resp, err = client.PostForm(server.URL+"/admin/users/"+strconv.FormatInt(plain.ID, 10)+"/block", nil)
	if err != nil {
		t.Fatalf("POST block: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("block = %d, want 303", resp.StatusCode)
	}
	if _, err := sessionSvc.Authenticate(ctx, userToken); err == nil {
		t.Error("blocked user session still authenticates")
	}
	var sessions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1`, plain.ID).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions after block = %d, want 0", sessions)
	}

	// Set and clear an override through the panel.
	resp, err = client.PostForm(server.URL+"/admin/override", url.Values{
		"user_id":                  {strconv.FormatInt(plain.ID, 10)},
		"max_keys_per_user":        {"1"},
		"max_days_limit":           {""},
		"max_bytes_per_chat":       {""},
		"max_total_bytes_per_user": {""},
	})
	if err != nil {
		t.Fatalf("POST /admin/override: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("override = %d, want 303", resp.StatusCode)
	}
	var overrides int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_overrides WHERE user_id = $1`, plain.ID).Scan(&overrides); err != nil {
		t.Fatalf("count overrides: %v", err)
	}
	if overrides != 1 {
		t.Fatalf("overrides = %d, want 1", overrides)
	}

	resp, err = client.PostForm(server.URL+"/admin/users/"+strconv.FormatInt(plain.ID, 10)+"/override/delete", nil)
	if err != nil {
		t.Fatalf("POST override/delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("override delete = %d, want 303", resp.StatusCode)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM user_overrides WHERE user_id = $1`, plain.ID).Scan(&overrides); err != nil {
		t.Fatalf("count overrides after delete: %v", err)
	}
	if overrides != 0 {
		t.Errorf("overrides after delete = %d, want 0", overrides)
	}

	// Deleting the user removes their chats and messages via cascade.
	if err := admins.SetBlocked(ctx, plain.ID, false); err != nil {
		t.Fatalf("unblock before delete: %v", err)
	}
	if _, err := service.NewKeyService(pool).Create(ctx, plain.ID, "Doomed"); err != nil {
		t.Fatalf("create key for doomed user: %v", err)
	}
	resp, err = client.PostForm(server.URL+"/admin/users/"+strconv.FormatInt(plain.ID, 10)+"/delete", nil)
	if err != nil {
		t.Fatalf("POST delete user: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete user = %d, want 303", resp.StatusCode)
	}
	var usersLeft int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE id = $1`, plain.ID).Scan(&usersLeft); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if usersLeft != 0 {
		t.Errorf("user still exists after delete")
	}
}
