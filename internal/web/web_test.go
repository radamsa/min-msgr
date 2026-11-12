package web_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/oauth"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/store/postgres"
	"github.com/dav/min-msgr/internal/testdb"
	"github.com/dav/min-msgr/internal/web"
)

type fakeSessions struct{}

func (fakeSessions) Create(context.Context, int64) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (fakeSessions) Authenticate(context.Context, string) (service.User, error) {
	return service.User{}, domain.ErrUnauthorized
}

func (fakeSessions) Delete(context.Context, string) error {
	return nil
}

type fakeOAuth struct{}

func (fakeOAuth) Login(context.Context, string, string, string, string) (service.User, error) {
	return service.User{}, nil
}

type fakeKeys struct{}

func (fakeKeys) Create(context.Context, int64, string) (service.CreateKeyResult, error) {
	return service.CreateKeyResult{}, nil
}

func (fakeKeys) List(context.Context, int64) (service.KeyOverview, error) {
	return service.KeyOverview{}, nil
}

func (fakeKeys) Rotate(context.Context, int64, int64) (service.CreateKeyResult, error) {
	return service.CreateKeyResult{}, nil
}

func (fakeKeys) UpdateChatLimits(context.Context, int64, int64, int, int64) (domain.Chat, error) {
	return domain.Chat{}, nil
}

type fakeArchives struct{}

func (fakeArchives) CreateAndDeleteChat(context.Context, int64, int64) (service.Archive, error) {
	return service.Archive{}, nil
}

func (fakeArchives) List(context.Context, int64) ([]service.Archive, error) {
	return nil, nil
}

func (fakeArchives) Open(context.Context, int64, int64) (service.Archive, *os.File, error) {
	return service.Archive{}, nil, domain.ErrNotFound
}

type fakeChats struct{}

func (fakeChats) ChatForUser(context.Context, int64, int64) (domain.Chat, error) {
	return domain.Chat{}, domain.ErrNotFound
}

func (fakeChats) ListMessagesForUser(context.Context, int64, int64, domain.ListParams) (domain.MessagePage, error) {
	return domain.MessagePage{}, nil
}

func (fakeChats) CreateUserMessage(context.Context, int64, int64, string, *string) (domain.Message, bool, error) {
	return domain.Message{}, false, domain.ErrNotFound
}

func newTestServer(t *testing.T, handler http.Handler) (*httptest.Server, *http.Client) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	client := server.Client()
	client.Jar = jar
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return server, client
}

func TestLoginPageWithoutProviders(t *testing.T) {
	handler, err := web.NewHandler(web.Options{
		OAuth: fakeOAuth{}, Sessions: fakeSessions{}, Keys: fakeKeys{},
		Archives: fakeArchives{}, Chats: fakeChats{},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server, client := newTestServer(t, handler.Routes())

	resp, err := client.Get(server.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "OAuth-провайдеры не настроены") {
		t.Errorf("login page does not mention missing providers: %s", body)
	}

	resp, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("GET / = %d Location %q, want 303 /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestOAuthLoginFlow(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()

	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.Form.Get("code") != "test-code" {
				http.Error(w, "bad code", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-token","token_type":"Bearer","expires_in":3600}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer access-token" {
				http.Error(w, "missing token", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"login":"octocat","email":"octocat@example.com"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer providerServer.Close()

	provider := oauth.NewGitHub(oauth.ProviderConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  "http://example.com/auth/github/callback",
		AuthURL:      providerServer.URL + "/authorize",
		TokenURL:     providerServer.URL + "/token",
		UserInfoURL:  providerServer.URL + "/user",
	})

	handler, err := web.NewHandler(web.Options{
		OAuth:     service.NewOAuthService(pool),
		Sessions:  service.NewSessionService(pool, time.Hour),
		Keys:      service.NewKeyService(pool),
		Archives:  service.NewArchiveService(pool, t.TempDir()),
		Chats:     postgres.New(pool),
		Providers: []*oauth.Provider{provider},
		Config:    web.Config{MaxMessageBytes: 64 * 1024},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server, client := newTestServer(t, handler.Routes())

	resp, err := client.Get(server.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	if body := readBody(t, resp); !strings.Contains(body, "Войти через GitHub") {
		t.Errorf("login page has no GitHub button: %s", body)
	}

	resp, err = client.Get(server.URL + "/auth/github")
	if err != nil {
		t.Fatalf("GET /auth/github: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("auth start status = %d, want 302", resp.StatusCode)
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse auth location: %v", err)
	}
	if !strings.HasPrefix(location.String(), providerServer.URL+"/authorize?") {
		t.Errorf("auth location = %q, want provider authorize URL", location)
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("auth location has no state")
	}

	// A callback with a wrong state must not log the user in.
	resp, err = client.Get(server.URL + "/auth/github/callback?code=test-code&state=wrong")
	if err != nil {
		t.Fatalf("GET callback with wrong state: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state status = %d, want 400", resp.StatusCode)
	}

	resp, err = client.Get(server.URL + "/auth/github/callback?code=test-code&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback = %d Location %q, want 303 /", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("home status = %d, want 200", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "octocat#") {
		t.Errorf("home page does not show the user: %s", body)
	}

	var users int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 {
		t.Errorf("users = %d, want 1", users)
	}
	var sessions int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Errorf("sessions = %d, want 1", sessions)
	}

	req, err := http.NewRequest(http.MethodPost, server.URL+"/logout", nil)
	if err != nil {
		t.Fatalf("create logout request: %v", err)
	}
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("logout = %d Location %q, want 303 /login", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET / after logout: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("after logout GET / = %d Location %q, want 303 /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatalf("count sessions after logout: %v", err)
	}
	if sessions != 0 {
		t.Errorf("sessions after logout = %d, want 0", sessions)
	}
}

func TestOAuthCallbackProviderError(t *testing.T) {
	provider := oauth.NewGitHub(oauth.ProviderConfig{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
	})
	handler, err := web.NewHandler(web.Options{
		OAuth: fakeOAuth{}, Sessions: fakeSessions{}, Keys: fakeKeys{},
		Archives: fakeArchives{}, Chats: fakeChats{},
		Providers: []*oauth.Provider{provider},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server, client := newTestServer(t, handler.Routes())
	resp, err := client.Get(server.URL + "/auth/github/callback?error=access_denied")
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Вход отменён") {
		t.Errorf("callback error page = %s", body)
	}
}

func TestKeyManagementFlow(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()

	user, err := service.NewUserService(pool).Create(ctx, "web-keys", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	sessionSvc := service.NewSessionService(pool, time.Hour)
	token, _, err := sessionSvc.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	archiveDir := t.TempDir()
	handler, err := web.NewHandler(web.Options{
		OAuth:    service.NewOAuthService(pool),
		Sessions: sessionSvc,
		Keys:     service.NewKeyService(pool),
		Archives: service.NewArchiveService(pool, archiveDir),
		Chats:    postgres.New(pool),
		Config:   web.Config{MaxMessageBytes: 64 * 1024},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server, client := newTestServer(t, handler.Routes())
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	client.Jar.SetCookies(serverURL, []*http.Cookie{{
		Name: "minmsgr_session", Value: token, Path: "/",
	}})

	resp, err := client.Get(server.URL + "/keys")
	if err != nil {
		t.Fatalf("GET /keys: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /keys status = %d, want 200", resp.StatusCode)
	}
	if body := readBody(t, resp); !strings.Contains(body, "Новый ключ") {
		t.Errorf("keys page = %s", body)
	}

	resp, err = client.PostForm(server.URL+"/keys", url.Values{"label": {"Agent One"}})
	if err != nil {
		t.Fatalf("POST /keys: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /keys status = %d, want 200", resp.StatusCode)
	}
	createdBody := readBody(t, resp)
	if !strings.Contains(createdBody, "Ключ создан") || !strings.Contains(createdBody, "Agent One") {
		t.Errorf("created page = %s", createdBody)
	}
	firstKey := extractKey(t, createdBody)

	var chatID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM chats WHERE user_id = $1 AND label = 'Agent One'`, user.ID).
		Scan(&chatID); err != nil {
		t.Fatalf("read chat: %v", err)
	}

	resp, err = client.Get(server.URL + "/keys")
	if err != nil {
		t.Fatalf("GET /keys after create: %v", err)
	}
	keysBody := readBody(t, resp)
	for _, want := range []string{"Agent One", "Ротировать ключ", "Отозвать"} {
		if !strings.Contains(keysBody, want) {
			t.Errorf("keys page missing %q: %s", want, keysBody)
		}
	}

	resp, err = client.PostForm(server.URL+"/keys/"+strconv.FormatInt(chatID, 10)+"/rotate", nil)
	if err != nil {
		t.Fatalf("POST rotate: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200", resp.StatusCode)
	}
	rotatedBody := readBody(t, resp)
	rotatedKey := extractKey(t, rotatedBody)
	if rotatedKey == firstKey {
		t.Error("rotated key equals the original key")
	}

	resp, err = client.PostForm(server.URL+"/keys/"+strconv.FormatInt(chatID, 10)+"/limits", url.Values{
		"days_limit":  {"7"},
		"bytes_limit": {"4096"},
	})
	if err != nil {
		t.Fatalf("POST limits: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/keys" {
		t.Fatalf("limits update = %d Location %q, want 303 /keys",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	var daysLimit int
	var bytesLimit int64
	if err := pool.QueryRow(ctx, `SELECT days_limit, bytes_limit FROM chats WHERE id = $1`, chatID).
		Scan(&daysLimit, &bytesLimit); err != nil {
		t.Fatalf("read chat limits: %v", err)
	}
	if daysLimit != 7 || bytesLimit != 4096 {
		t.Errorf("chat limits = %d days %d bytes, want 7 days 4096 bytes", daysLimit, bytesLimit)
	}

	resp, err = client.PostForm(server.URL+"/keys/"+strconv.FormatInt(chatID, 10)+"/revoke", nil)
	if err != nil {
		t.Fatalf("POST revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/keys" {
		t.Fatalf("revoke = %d Location %q, want 303 /keys", resp.StatusCode, resp.Header.Get("Location"))
	}

	var archiveID int64
	if err := pool.QueryRow(ctx, `SELECT id FROM archives WHERE user_id = $1`, user.ID).Scan(&archiveID); err != nil {
		t.Fatalf("read archive: %v", err)
	}

	resp, err = client.Get(server.URL + "/keys")
	if err != nil {
		t.Fatalf("GET /keys after revoke: %v", err)
	}
	archivesBody := readBody(t, resp)
	if !strings.Contains(archivesBody, "Скачать .zip") || !strings.Contains(archivesBody, "Agent One") {
		t.Errorf("archives page = %s", archivesBody)
	}

	resp, err = client.Get(server.URL + "/archives/" + strconv.FormatInt(archiveID, 10))
	if err != nil {
		t.Fatalf("GET archive: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive status = %d, want 200", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "application/zip" {
		t.Errorf("archive Content-Type = %q, want application/zip", contentType)
	}
	archiveBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read archive body: %v", err)
	}
	if len(archiveBody) < 2 || string(archiveBody[:2]) != "PK" {
		t.Errorf("archive body is not a zip file: %q", archiveBody)
	}

	resp, err = client.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if body := readBody(t, resp); !strings.Contains(body, "Чатов пока нет") {
		t.Errorf("chats page after revoke = %s", body)
	}
}

func TestKeyPagesRequireLogin(t *testing.T) {
	handler, err := web.NewHandler(web.Options{
		OAuth: fakeOAuth{}, Sessions: fakeSessions{}, Keys: fakeKeys{},
		Archives: fakeArchives{}, Chats: fakeChats{},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server, client := newTestServer(t, handler.Routes())

	for _, path := range []string{"/", "/keys"} {
		resp, err := client.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("GET %s = %d Location %q, want 303 /login",
				path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func extractKey(t *testing.T, body string) string {
	t.Helper()
	const prefix = `<code class="key">`
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("page has no key block: %s", body)
	}
	start += len(prefix)
	end := strings.Index(body[start:], "</code>")
	if end < 0 {
		t.Fatalf("page has an unterminated key block: %s", body)
	}
	key := strings.TrimSpace(body[start : start+end])
	if !strings.HasPrefix(key, "sk_") {
		t.Errorf("extracted key = %q, want sk_ prefix", key)
	}
	return key
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
