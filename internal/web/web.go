// Package web implements the browser-facing pages and the OAuth login flow.
// Pages are rendered on the server with html/template; the future chat UI will
// extend this package with WebSocket handlers.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/markdown"
	"github.com/dav/min-msgr/internal/oauth"
	"github.com/dav/min-msgr/internal/realtime"
	"github.com/dav/min-msgr/internal/service"
)

const (
	sessionCookieName = "minmsgr_session"
	stateCookieName   = "minmsgr_oauth_state"
	stateCookieTTL    = 10 * time.Minute
)

//go:embed templates/*.html
var templateFiles embed.FS

// OAuthService is the subset of service.OAuthService used by the web layer.
type OAuthService interface {
	Login(ctx context.Context, provider, providerUserID, login, email string) (service.User, error)
}

// SessionService is the subset of service.SessionService used by the web layer.
type SessionService interface {
	Create(ctx context.Context, userID int64) (string, time.Time, error)
	Authenticate(ctx context.Context, token string) (service.User, error)
	Delete(ctx context.Context, token string) error
}

// KeyService is the subset of service.KeyService used by the web layer.
type KeyService interface {
	Create(ctx context.Context, userID int64, label string) (service.CreateKeyResult, error)
	List(ctx context.Context, userID int64) (service.KeyOverview, error)
	Rotate(ctx context.Context, userID, chatID int64) (service.CreateKeyResult, error)
	UpdateChatLimits(ctx context.Context, userID, chatID int64, daysLimit int, bytesLimit int64) (domain.Chat, error)
}

// ArchiveService is the subset of service.ArchiveService used by the web layer.
type ArchiveService interface {
	CreateAndDeleteChat(ctx context.Context, userID, chatID int64) (service.Archive, error)
	List(ctx context.Context, userID int64) ([]service.Archive, error)
	Open(ctx context.Context, userID, archiveID int64) (service.Archive, *os.File, error)
}

// ChatStore is the persistence subset used by the web chat and WebSocket.
type ChatStore interface {
	ChatForUser(ctx context.Context, userID, chatID int64) (domain.Chat, error)
	ListMessagesForUser(ctx context.Context, userID, chatID int64, params domain.ListParams) (domain.MessagePage, error)
	CreateUserMessage(
		ctx context.Context,
		userID, chatID int64,
		body string,
		clientMessageID *string,
	) (domain.Message, bool, error)
}

// Config contains web-specific settings.
type Config struct {
	CookieSecure    bool
	MaxMessageBytes int
	PageLimit       int
}

// Trimmer enforces storage limits after a message is stored.
type Trimmer interface {
	TrimAfterMessage(ctx context.Context, chatID int64) ([]domain.TrimResult, error)
}

// AdminService is the subset of service.AdminService used by the admin panel.
type AdminService interface {
	Settings(ctx context.Context) (service.AdminSettingsView, error)
	UpdateSettings(
		ctx context.Context,
		maxKeys, maxDays int,
		maxBytesPerChat, maxTotalBytes int64,
	) (service.AdminSettingsView, error)
	Users(ctx context.Context) ([]service.UserListItem, error)
	SetBlocked(ctx context.Context, userID int64, blocked bool) error
	DeleteUser(ctx context.Context, userID int64) error
	Overrides(ctx context.Context) ([]service.UserOverrideItem, error)
	SetOverride(
		ctx context.Context,
		userID int64,
		maxKeys, maxDays *int,
		maxBytesPerChat, maxTotalBytes *int64,
	) error
	DeleteOverride(ctx context.Context, userID int64) error
}

// Options bundles the web handler dependencies.
type Options struct {
	OAuth     OAuthService
	Sessions  SessionService
	Keys      KeyService
	Archives  ArchiveService
	Chats     ChatStore
	Trimmer   Trimmer
	Hub       *realtime.Hub
	Admins    AdminService
	Providers []*oauth.Provider
	Config    Config
}

const defaultPageLimit = 50

// Handler serves the login flow, chat pages, key management, and WebSocket.
type Handler struct {
	oauth           OAuthService
	sessions        SessionService
	keys            KeyService
	archives        ArchiveService
	chats           ChatStore
	trimmer         Trimmer
	hub             *realtime.Hub
	admins          AdminService
	providers       map[string]*oauth.Provider
	providerList    []*oauth.Provider
	templates       *template.Template
	renderer        *markdown.Renderer
	cookieSecure    bool
	maxMessageBytes int
	pageLimit       int
}

func NewHandler(opts Options) (*Handler, error) {
	templates, err := template.ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}

	pageLimit := opts.Config.PageLimit
	if pageLimit <= 0 {
		pageLimit = defaultPageLimit
	}
	maxMessageBytes := opts.Config.MaxMessageBytes
	if maxMessageBytes <= 0 {
		maxMessageBytes = 64 * 1024
	}

	h := &Handler{
		oauth:           opts.OAuth,
		sessions:        opts.Sessions,
		keys:            opts.Keys,
		archives:        opts.Archives,
		chats:           opts.Chats,
		trimmer:         opts.Trimmer,
		hub:             opts.Hub,
		admins:          opts.Admins,
		providers:       make(map[string]*oauth.Provider, len(opts.Providers)),
		providerList:    opts.Providers,
		templates:       templates,
		renderer:        markdown.New(),
		cookieSecure:    opts.Config.CookieSecure,
		maxMessageBytes: maxMessageBytes,
		pageLimit:       pageLimit,
	}
	for _, provider := range opts.Providers {
		h.providers[provider.Name()] = provider
	}
	return h, nil
}

// Routes returns the browser-facing routes. The API is mounted separately.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", h.handleChats)
	mux.HandleFunc("GET /login", h.handleLogin)
	mux.HandleFunc("GET /auth/{provider}", h.handleAuthStart)
	mux.HandleFunc("GET /auth/{provider}/callback", h.handleAuthCallback)
	mux.HandleFunc("POST /logout", h.handleLogout)
	mux.HandleFunc("GET /keys", h.handleKeys)
	mux.HandleFunc("POST /keys", h.handleCreateKey)
	mux.HandleFunc("POST /keys/{chatID}/rotate", h.handleRotateKey)
	mux.HandleFunc("POST /keys/{chatID}/limits", h.handleUpdateChatLimits)
	mux.HandleFunc("POST /keys/{chatID}/revoke", h.handleRevokeKey)
	mux.HandleFunc("GET /archives/{archiveID}", h.handleDownloadArchive)
	mux.HandleFunc("GET /admin", h.requireAdmin(h.handleAdminPanel))
	mux.HandleFunc("POST /admin/settings", h.requireAdmin(h.handleAdminUpdateSettings))
	mux.HandleFunc("POST /admin/users/{userID}/block", h.requireAdmin(h.handleAdminBlock))
	mux.HandleFunc("POST /admin/users/{userID}/unblock", h.requireAdmin(h.handleAdminUnblock))
	mux.HandleFunc("POST /admin/users/{userID}/delete", h.requireAdmin(h.handleAdminDeleteUser))
	mux.HandleFunc("POST /admin/users/{userID}/override", h.requireAdmin(h.handleAdminSetOverride))
	mux.HandleFunc("POST /admin/users/{userID}/override/delete", h.requireAdmin(h.handleAdminDeleteOverride))
	mux.HandleFunc("POST /admin/override", h.requireAdmin(h.handleAdminSetOverrideForm))
	mux.HandleFunc("GET /chats/{chatID}", h.handleChatPage)
	mux.HandleFunc("GET /ws", h.handleWebSocket)
	mux.HandleFunc("GET /static/chat.js", h.handleChatJS)
	return mux
}

func (h *Handler) handleChats(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	overview, err := h.keys.List(r.Context(), user.ID)
	if err != nil {
		slog.Error("web: list chats", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, http.StatusOK, "chats.html", struct {
		User  service.User
		Chats []service.ChatSummary
	}{User: user, Chats: overview.Chats})
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if _, err := h.currentUser(r); err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	h.renderLogin(w, http.StatusOK, "")
}

func (h *Handler) handleKeys(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	h.renderKeys(w, r, user, http.StatusOK, "")
}

func (h *Handler) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	label := strings.TrimSpace(r.FormValue("label"))
	if label == "" || len(label) > 100 {
		h.renderKeys(w, r, user, http.StatusBadRequest, "Метка должна быть от 1 до 100 символов.")
		return
	}

	result, err := h.keys.Create(r.Context(), user.ID, label)
	switch {
	case errors.Is(err, domain.ErrKeyLimit):
		h.renderKeys(w, r, user, http.StatusConflict, "Достигнут лимит ключей.")
	case errors.Is(err, domain.ErrUnauthorized):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case err != nil:
		slog.Error("web: create key", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	default:
		h.renderKeyCreated(w, user, result)
	}
}

func (h *Handler) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	chatID, ok := parseID(r.PathValue("chatID"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	result, err := h.keys.Rotate(r.Context(), user.ID, chatID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, domain.ErrUnauthorized):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case err != nil:
		slog.Error("web: rotate key", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	default:
		h.renderKeyCreated(w, user, result)
	}
}

func (h *Handler) handleUpdateChatLimits(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	chatID, ok := parseID(r.PathValue("chatID"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("days_limit")))
	bytes, badBytes := strconv.ParseInt(strings.TrimSpace(r.FormValue("bytes_limit")), 10, 64)
	if (r.FormValue("days_limit") != "" && err != nil) ||
		(r.FormValue("bytes_limit") != "" && badBytes != nil) {
		h.renderKeys(w, r, user, http.StatusBadRequest, "Лимиты должны быть целыми числами.")
		return
	}

	_, err = h.keys.UpdateChatLimits(r.Context(), user.ID, chatID, days, bytes)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, domain.ErrUnauthorized):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case err != nil:
		slog.Error("web: update chat limits", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	default:
		http.Redirect(w, r, "/keys", http.StatusSeeOther)
	}
}

func (h *Handler) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	chatID, ok := parseID(r.PathValue("chatID"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	_, err := h.archives.CreateAndDeleteChat(r.Context(), user.ID, chatID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, domain.ErrConflict):
		// The chat was already archived by a concurrent request.
		http.Redirect(w, r, "/keys", http.StatusSeeOther)
	case err != nil:
		slog.Error("web: revoke key", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	default:
		if h.hub != nil {
			h.hub.Publish(chatID, realtime.Event{
				Type:    "chat.deleted",
				Payload: realtime.ChatDeleted{ChatID: chatID},
			})
		}
		http.Redirect(w, r, "/keys", http.StatusSeeOther)
	}
}

func (h *Handler) handleDownloadArchive(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	archiveID, ok := parseID(r.PathValue("archiveID"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	archive, file, err := h.archives.Open(r.Context(), user.ID, archiveID)
	if errors.Is(err, domain.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("web: open archive", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	filename := filepath.Base(archive.FilePath)
	w.Header().Set("Content-Type", "application/zip")
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename}); disposition != "" {
		w.Header().Set("Content-Disposition", disposition)
	}
	http.ServeContent(w, r, filename, archive.CreatedAt, file)
}

func (h *Handler) renderKeys(
	w http.ResponseWriter,
	r *http.Request,
	user service.User,
	status int,
	message string,
) {
	overview, err := h.keys.List(r.Context(), user.ID)
	if err != nil {
		slog.Error("web: list keys", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	archives, err := h.archives.List(r.Context(), user.ID)
	if err != nil {
		slog.Error("web: list archives", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, status, "keys.html", struct {
		User                 service.User
		Chats                []service.ChatSummary
		Archives             []service.Archive
		MaxKeys              int
		MaxDaysLimit         int
		MaxBytesPerChat      int64
		MaxTotalBytesPerUser int64
		UsedBytesPerUser     int64
		Error                string
	}{
		User:                 user,
		Chats:                overview.Chats,
		Archives:             archives,
		MaxKeys:              overview.MaxKeys,
		MaxDaysLimit:         overview.MaxDaysLimit,
		MaxBytesPerChat:      overview.MaxBytesPerChat,
		MaxTotalBytesPerUser: overview.MaxTotalBytesPerUser,
		UsedBytesPerUser:     overview.UsedBytesPerUser,
		Error:                message,
	})
}

func (h *Handler) renderKeyCreated(w http.ResponseWriter, user service.User, result service.CreateKeyResult) {
	h.render(w, http.StatusOK, "key_created.html", struct {
		User     service.User
		Label    string
		ChatID   int64
		PlainKey string
	}{
		User:     user,
		Label:    result.Label,
		ChatID:   result.ChatID,
		PlainKey: result.PlainKey,
	})
}

func (h *Handler) requireUser(w http.ResponseWriter, r *http.Request) (service.User, bool) {
	user, err := h.currentUser(r)
	if errors.Is(err, domain.ErrUnauthorized) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return service.User{}, false
	}
	if err != nil {
		slog.Error("web: authenticate session", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return service.User{}, false
	}
	return user, true
}

func (h *Handler) handleAuthStart(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.providers[r.PathValue("provider")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	state, err := randomToken()
	if err != nil {
		slog.Error("web: generate oauth state", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    provider.Name() + ":" + state,
		Path:     "/",
		MaxAge:   int(stateCookieTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, provider.AuthCodeURL(state), http.StatusFound)
}

func (h *Handler) handleAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider, ok := h.providers[r.PathValue("provider")]
	if !ok {
		http.NotFound(w, r)
		return
	}

	query := r.URL.Query()
	if query.Get("error") != "" {
		h.renderLogin(w, http.StatusBadRequest, "Вход отменён или отклонён провайдером.")
		return
	}

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || !validState(stateCookie.Value, provider.Name(), query.Get("state")) {
		h.renderLogin(w, http.StatusBadRequest, "Сессия входа истекла. Попробуйте снова.")
		return
	}
	h.clearCookie(w, stateCookieName)

	code := query.Get("code")
	if code == "" {
		h.renderLogin(w, http.StatusBadRequest, "Провайдер не вернул код авторизации.")
		return
	}

	token, err := provider.Exchange(r.Context(), code)
	if err != nil {
		slog.Error("web: exchange oauth code", "provider", provider.Name(), "error", err)
		h.renderLogin(w, http.StatusBadGateway, "Не удалось получить профиль. Попробуйте снова.")
		return
	}
	identity, err := provider.FetchIdentity(r.Context(), token)
	if err != nil {
		slog.Error("web: fetch oauth identity", "provider", provider.Name(), "error", err)
		h.renderLogin(w, http.StatusBadGateway, "Не удалось получить профиль. Попробуйте снова.")
		return
	}

	user, err := h.oauth.Login(
		r.Context(), provider.Name(), identity.ProviderUserID, identity.Login, identity.Email,
	)
	if errors.Is(err, domain.ErrBlocked) {
		h.renderLogin(w, http.StatusForbidden, "Аккаунт заблокирован.")
		return
	}
	if err != nil {
		slog.Error("web: register oauth user", "provider", provider.Name(), "error", err)
		h.renderLogin(w, http.StatusInternalServerError, "Не удалось войти. Попробуйте снова.")
		return
	}

	sessionToken, expiresAt, err := h.sessions.Create(r.Context(), user.ID)
	if err != nil {
		slog.Error("web: create session", "error", err)
		h.renderLogin(w, http.StatusInternalServerError, "Не удалось войти. Попробуйте снова.")
		return
	}
	h.setSessionCookie(w, sessionToken, expiresAt)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		if err := h.sessions.Delete(r.Context(), cookie.Value); err != nil {
			slog.Error("web: delete session", "error", err)
		}
	}
	h.clearCookie(w, sessionCookieName)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (h *Handler) currentUser(r *http.Request) (service.User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return service.User{}, domain.ErrUnauthorized
	}
	return h.sessions.Authenticate(r.Context(), cookie.Value)
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) renderLogin(w http.ResponseWriter, status int, message string) {
	providers := make([]struct {
		Name  string
		Title string
	}, 0, len(h.providerList))
	for _, provider := range h.providerList {
		providers = append(providers, struct {
			Name  string
			Title string
		}{Name: provider.Name(), Title: providerTitle(provider.Name())})
	}
	h.render(w, status, "login.html", struct {
		User      service.User
		Providers []struct {
			Name  string
			Title string
		}
		Error string
	}{Providers: providers, Error: message})
}

func (h *Handler) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("web: render template", "template", name, "error", err)
	}
}

func providerTitle(name string) string {
	switch name {
	case "github":
		return "GitHub"
	case "google":
		return "Google"
	case "yandex":
		return "Yandex"
	default:
		return name
	}
}

func validState(cookieValue, provider, state string) bool {
	if state == "" {
		return false
	}
	prefix := provider + ":"
	if !strings.HasPrefix(cookieValue, prefix) {
		return false
	}
	expected := strings.TrimPrefix(cookieValue, prefix)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(state)) == 1
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func parseID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
