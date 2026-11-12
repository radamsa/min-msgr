package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
)

// adminUserKey is the context key for the authenticated service.User.
type adminUserKey struct{}

// requireAdmin wraps a handler so only logged-in admins can reach it. Non-
// admins (including anonymous users) are redirected to the login page.
func (h *Handler) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := h.requireUser(w, r)
		if !ok {
			return
		}
		if !user.IsAdmin {
			http.Error(w, "только для администраторов", http.StatusForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), adminUserKey{}, user)))
	}
}

func (h *Handler) handleAdminPanel(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(adminUserKey{}).(service.User)
	settings, err := h.admins.Settings(r.Context())
	if err != nil {
		slog.Error("web: admin settings", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	users, err := h.admins.Users(r.Context())
	if err != nil {
		slog.Error("web: admin users", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	overrides, err := h.admins.Overrides(r.Context())
	if err != nil {
		slog.Error("web: admin overrides", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, http.StatusOK, "admin.html", struct {
		User      service.User
		Settings  service.AdminSettingsView
		Users     []service.UserListItem
		Overrides []service.UserOverrideItem
		Error     string
	}{
		User:      user,
		Settings:  settings,
		Users:     users,
		Overrides: overrides,
		Error:     r.URL.Query().Get("error"),
	})
}

func (h *Handler) handleAdminUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if !h.applyAdminSettings(w, r) {
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) applyAdminSettings(w http.ResponseWriter, r *http.Request) bool {
	maxKeys, ok := adminInt(w, r, "max_keys_per_user")
	if !ok {
		return false
	}
	maxDays, ok := adminInt(w, r, "max_days_limit")
	if !ok {
		return false
	}
	maxBytesPerChat, ok := adminInt64(w, r, "max_bytes_per_chat")
	if !ok {
		return false
	}
	maxTotalBytes, ok := adminInt64(w, r, "max_total_bytes_per_user")
	if !ok {
		return false
	}
	if _, err := h.admins.UpdateSettings(r.Context(), maxKeys, maxDays, maxBytesPerChat, maxTotalBytes); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return false
		}
		slog.Error("web: admin settings update", "error", err)
		http.Error(w, "Настройки должны быть положительными числами", http.StatusBadRequest)
		return false
	}
	return true
}

func (h *Handler) handleAdminBlock(w http.ResponseWriter, r *http.Request) {
	h.handleAdminSetBlocked(w, r, true)
}

func (h *Handler) handleAdminUnblock(w http.ResponseWriter, r *http.Request) {
	h.handleAdminSetBlocked(w, r, false)
}

func (h *Handler) handleAdminSetBlocked(w http.ResponseWriter, r *http.Request, blocked bool) {
	userID, ok := parseID(r.PathValue("userID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.admins.SetBlocked(r.Context(), userID, blocked); err != nil {
		h.handleAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseID(r.PathValue("userID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.admins.DeleteUser(r.Context(), userID); err != nil {
		h.handleAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) handleAdminSetOverride(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseID(r.PathValue("userID"))
	if !ok {
		http.NotFound(w, r)
		return
	}

	maxKeys, ok := adminOptionalInt(w, r, "max_keys_per_user")
	if !ok {
		return
	}
	maxDays, ok := adminOptionalInt(w, r, "max_days_limit")
	if !ok {
		return
	}
	maxBytesPerChat, ok := adminOptionalInt64(w, r, "max_bytes_per_chat")
	if !ok {
		return
	}
	maxTotalBytes, ok := adminOptionalInt64(w, r, "max_total_bytes_per_user")
	if !ok {
		return
	}

	if err := h.admins.SetOverride(r.Context(), userID, maxKeys, maxDays, maxBytesPerChat, maxTotalBytes); err != nil {
		h.handleAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) handleAdminDeleteOverride(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseID(r.PathValue("userID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.admins.DeleteOverride(r.Context(), userID); err != nil {
		h.handleAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminSetOverride processes the free-form override form where the user
// ID is a field, not a path segment.
func (h *Handler) handleAdminSetOverrideForm(w http.ResponseWriter, r *http.Request) {
	userID, ok := parseID(r.FormValue("user_id"))
	if !ok {
		http.Error(w, "user_id должен быть положительным числом", http.StatusBadRequest)
		return
	}

	maxKeys, ok := adminOptionalInt(w, r, "max_keys_per_user")
	if !ok {
		return
	}
	maxDays, ok := adminOptionalInt(w, r, "max_days_limit")
	if !ok {
		return
	}
	maxBytesPerChat, ok := adminOptionalInt64(w, r, "max_bytes_per_chat")
	if !ok {
		return
	}
	maxTotalBytes, ok := adminOptionalInt64(w, r, "max_total_bytes_per_user")
	if !ok {
		return
	}

	if err := h.admins.SetOverride(r.Context(), userID, maxKeys, maxDays, maxBytesPerChat, maxTotalBytes); err != nil {
		h.handleAdminError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// handleAdminError distinguishes "user vanished" from real failures and keeps
// the panel reachable either way.
func (h *Handler) handleAdminError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	slog.Error("web: admin action", "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func adminInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	value, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
	if err != nil || value <= 0 {
		http.Error(w, name+" должен быть положительным числом", http.StatusBadRequest)
		return 0, false
	}
	return value, true
}

func adminInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	value, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	if err != nil || value <= 0 {
		http.Error(w, name+" должен быть положительным числом", http.StatusBadRequest)
		return 0, false
	}
	return value, true
}

// adminOptionalInt parses an override field where empty means "use default".
func adminOptionalInt(w http.ResponseWriter, r *http.Request, name string) (*int, bool) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return nil, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		http.Error(w, name+" должен быть положительным числом или пустым", http.StatusBadRequest)
		return nil, false
	}
	return &value, true
}

func adminOptionalInt64(w http.ResponseWriter, r *http.Request, name string) (*int64, bool) {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return nil, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		http.Error(w, name+" должен быть положительным числом или пустым", http.StatusBadRequest)
		return nil, false
	}
	return &value, true
}
