package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dav/min-msgr/internal/auth"
	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/markdown"
	"github.com/dav/min-msgr/internal/realtime"
	"github.com/dav/min-msgr/internal/store"
)

var agentNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,16}$`)

// Notifier receives chat events for connected web clients.
type Notifier interface {
	Publish(chatID int64, event realtime.Event)
}

// Trimmer enforces storage limits after a message is stored.
type Trimmer interface {
	TrimAfterMessage(ctx context.Context, chatID int64) ([]domain.TrimResult, error)
}

// Config contains API-specific limits.
type Config struct {
	MaxMessageBytes int
	MaxAPILimit     int
	DefaultAPILimit int
	Notifier        Notifier
	Trimmer         Trimmer
}

type Handler struct {
	store    store.Store
	cfg      Config
	renderer *markdown.Renderer
}

// NewHandler returns the agent REST API. Only agents use this API; the web UI
// uses WebSocket.
func NewHandler(st store.Store, cfg Config) http.Handler {
	h := &Handler{store: st, cfg: cfg, renderer: markdown.New()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/messages", h.handlePostMessage)
	mux.HandleFunc("GET /api/v1/messages", h.handleListMessages)
	mux.HandleFunc("GET /healthz", h.handleHealth)
	return mux
}

func (h *Handler) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type postMessageRequest struct {
	AgentName       string `json:"agent_name"`
	Body            string `json:"body"`
	ClientMessageID string `json:"client_message_id"`
}

func (h *Handler) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	authContext, err := h.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid API key")
		return
	}

	// The decoded message is limited by MaxMessageBytes. JSON escaping can
	// make the wire body larger, so allow a bounded amount of overhead.
	maxBody := int64(h.cfg.MaxMessageBytes)*2 + 64*1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req postMessageRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must be valid JSON")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_json", "request body must contain one JSON object")
		return
	}

	agentName := req.AgentName
	if !agentNamePattern.MatchString(agentName) {
		writeError(w, http.StatusBadRequest, "invalid_agent_name", "agent_name must be 1-16 characters: letters, digits, _ or -")
		return
	}

	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "empty_body", "body must not be empty")
		return
	}
	if len(body) > h.cfg.MaxMessageBytes {
		writeError(w, http.StatusBadRequest, "message_too_large", fmt.Sprintf("body must not exceed %d bytes", h.cfg.MaxMessageBytes))
		return
	}

	var clientMessageID *string
	if id := strings.TrimSpace(req.ClientMessageID); id != "" {
		if len(id) > 128 {
			writeError(w, http.StatusBadRequest, "invalid_client_message_id", "client_message_id must not exceed 128 bytes")
			return
		}
		clientMessageID = &id
	}

	message, created, err := h.store.CreateAgentMessage(
		r.Context(), authContext.ChatID, agentName, body, clientMessageID,
	)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}

	if created && h.cfg.Notifier != nil {
		h.notifyMessageCreated(authContext.ChatID, message)
	}
	if created && h.cfg.Trimmer != nil {
		h.trimAfterMessage(r.Context(), authContext.ChatID)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, message)
}

// trimAfterMessage enforces byte limits without tying the maintenance work to
// the request context: a client disconnect must not leave the chat over limit.
func (h *Handler) trimAfterMessage(parent context.Context, chatID int64) {
	ctx := context.WithoutCancel(parent)
	trims, err := h.cfg.Trimmer.TrimAfterMessage(ctx, chatID)
	if err != nil {
		slog.Error("api: trim messages", "chat_id", chatID, "error", err)
		return
	}
	if h.cfg.Notifier == nil {
		return
	}
	for _, trim := range trims {
		h.cfg.Notifier.Publish(trim.ChatID, realtime.Event{
			Type: "history.trimmed",
			Payload: realtime.HistoryTrimmed{
				ChatID:     trim.ChatID,
				Deleted:    trim.Deleted,
				ThroughSeq: trim.ThroughSeq,
			},
		})
	}
}

func (h *Handler) notifyMessageCreated(chatID int64, message domain.Message) {
	html, err := h.renderer.Render(message.Body)
	if err != nil {
		slog.Error("api: render message for websocket", "error", err)
		return
	}
	h.cfg.Notifier.Publish(chatID, realtime.Event{
		Type: "message.created",
		Payload: realtime.MessageCreated{
			ChatID: chatID,
			Message: realtime.MessageView{
				Seq:       message.Seq,
				Author:    message.Author,
				Body:      message.Body,
				HTML:      html,
				CreatedAt: message.CreatedAt,
			},
		},
	})
}

type listMessagesResponse struct {
	Messages   []domain.Message `json:"messages"`
	NextCursor *string          `json:"next_cursor"`
	Limit      int              `json:"limit"`
}

func (h *Handler) handleListMessages(w http.ResponseWriter, r *http.Request) {
	authContext, err := h.authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid API key")
		return
	}

	query := r.URL.Query()
	limit, err := h.parseLimit(query.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}

	var since *time.Time
	if raw := query.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_since", "since must be an RFC3339 timestamp")
			return
		}
		since = &parsed
	}

	params := domain.ListParams{Limit: limit, Since: since}
	if raw := query.Get("cursor"); raw != "" {
		cursor, err := decodeCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is invalid")
			return
		}
		if since != nil {
			if cursor.Direction != cursorForward {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor does not match forward pagination")
				return
			}
			params.AfterSeq = &cursor.Seq
		} else {
			if cursor.Direction != cursorBackward {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor does not match backward pagination")
				return
			}
			params.BeforeSeq = &cursor.Seq
		}
	}

	page, err := h.store.ListMessages(r.Context(), authContext.ChatID, params)
	if err != nil {
		h.writeStoreError(w, err)
		return
	}

	messages := page.Messages
	if messages == nil {
		messages = []domain.Message{}
	}

	var nextCursor *string
	if page.HasMore && len(messages) > 0 {
		var encoded string
		if since != nil {
			encoded = encodeCursor(messages[len(messages)-1].Seq, cursorForward)
		} else {
			encoded = encodeCursor(messages[0].Seq, cursorBackward)
		}
		nextCursor = &encoded
	}

	writeJSON(w, http.StatusOK, listMessagesResponse{
		Messages:   messages,
		NextCursor: nextCursor,
		Limit:      limit,
	})
}

func (h *Handler) parseLimit(raw string) (int, error) {
	if raw == "" {
		return h.cfg.DefaultAPILimit, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer")
	}
	if value > h.cfg.MaxAPILimit {
		value = h.cfg.MaxAPILimit
	}
	return value, nil
}

func (h *Handler) authenticate(r *http.Request) (domain.AuthContext, error) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return domain.AuthContext{}, domain.ErrUnauthorized
	}
	key := parts[1]
	if !auth.ValidFormat(key) {
		return domain.AuthContext{}, domain.ErrUnauthorized
	}
	return h.store.AuthenticateAPIKey(r.Context(), auth.Hash(key))
}

func (h *Handler) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrUnauthorized):
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid API key")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", "message conflicts with an existing message")
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	default:
		slog.Error("api request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Error: errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write json response", "error", err)
	}
}
