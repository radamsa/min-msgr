package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/dav/min-msgr/internal/cursor"
	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/realtime"
	"github.com/dav/min-msgr/internal/service"
)

const (
	wsWriteTimeout = 10 * time.Second
	wsPingInterval = 30 * time.Second
)

//go:embed static/chat.js
var chatJS []byte

type incomingEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type outgoingEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

type subscribePayload struct {
	ChatID  int64  `json:"chat_id"`
	Since   string `json:"since"`
	Initial bool   `json:"initial"`
}

type beforePayload struct {
	ChatID int64  `json:"chat_id"`
	Cursor string `json:"cursor"`
}

type sendPayload struct {
	ChatID          int64  `json:"chat_id"`
	Body            string `json:"body"`
	ClientMessageID string `json:"client_message_id"`
}

func (h *Handler) handleChatJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(chatJS)
}

func (h *Handler) handleChatPage(w http.ResponseWriter, r *http.Request) {
	user, ok := h.requireUser(w, r)
	if !ok {
		return
	}
	chatID, ok := parseID(r.PathValue("chatID"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	chat, err := h.chats.ChatForUser(r.Context(), user.ID, chatID)
	if errors.Is(err, domain.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		slog.Error("web: load chat", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	h.render(w, http.StatusOK, "chat.html", struct {
		User            service.User
		Chat            domain.Chat
		MaxMessageBytes int
	}{
		User:            user,
		Chat:            chat,
		MaxMessageBytes: h.maxMessageBytes,
	})
}

func (h *Handler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	user, err := h.currentUser(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	chatID, ok := parseID(r.URL.Query().Get("chat_id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := h.chats.ChatForUser(r.Context(), user.ID, chatID); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("web: websocket chat lookup", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		slog.Debug("web: websocket accept failed", "error", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(int64(h.maxMessageBytes)*2 + 64*1024)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	events := make(chan realtime.Event, 64)
	var subscription *realtime.Subscription
	if h.hub != nil {
		subscription = h.hub.Subscribe(chatID)
		defer subscription.Close()
		go forwardEvents(ctx, subscription.Events(), events)
	}

	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		if err := h.writeEvents(ctx, conn, events); err != nil && ctx.Err() == nil {
			slog.Debug("web: websocket write loop stopped", "error", err)
		}
		cancel()
		_ = conn.CloseNow()
	}()

	if err := h.readEvents(ctx, conn, user.ID, chatID, events); err != nil && ctx.Err() == nil {
		slog.Debug("web: websocket read loop stopped", "error", err)
	}
	cancel()
	_ = conn.CloseNow()
	<-writeDone
}

func forwardEvents(ctx context.Context, in <-chan realtime.Event, out chan<- realtime.Event) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-in:
			if !ok {
				return
			}
			select {
			case out <- event:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (h *Handler) writeEvents(
	ctx context.Context,
	conn *websocket.Conn,
	events <-chan realtime.Event,
) error {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := conn.Ping(pingCtx)
			cancel()
			if err != nil {
				return err
			}
		case event, ok := <-events:
			if !ok {
				return nil
			}
			payload, err := json.Marshal(outgoingEvent{Type: event.Type, Payload: event.Payload})
			if err != nil {
				return fmt.Errorf("encode websocket event: %w", err)
			}
			writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err = conn.Write(writeCtx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

func (h *Handler) readEvents(
	ctx context.Context,
	conn *websocket.Conn,
	userID, chatID int64,
	events chan<- realtime.Event,
) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var incoming incomingEvent
		if err := json.Unmarshal(data, &incoming); err != nil {
			if err := sendEvent(ctx, events, errorEvent("", "invalid_json", "event must be valid JSON")); err != nil {
				return err
			}
			continue
		}

		switch incoming.Type {
		case "messages.subscribe":
			if err := h.handleSubscribe(ctx, userID, chatID, incoming.Payload, events); err != nil {
				return err
			}
		case "messages.before":
			if err := h.handleBefore(ctx, userID, chatID, incoming.Payload, events); err != nil {
				return err
			}
		case "message.send":
			if err := h.handleSend(ctx, userID, chatID, incoming.Payload, events); err != nil {
				return err
			}
		default:
			if err := sendEvent(ctx, events, errorEvent("", "unknown_event", "unknown event type")); err != nil {
				return err
			}
		}
	}
}

func (h *Handler) handleSubscribe(
	ctx context.Context,
	userID, chatID int64,
	raw json.RawMessage,
	events chan<- realtime.Event,
) error {
	var payload subscribePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return sendEvent(ctx, events, errorEvent("", "invalid_payload", "messages.subscribe payload is invalid"))
	}
	if payload.ChatID != 0 && payload.ChatID != chatID {
		return sendEvent(ctx, events, errorEvent("", "invalid_chat", "chat_id does not match the connection"))
	}

	var since *time.Time
	if payload.Since != "" {
		parsed, err := time.Parse(time.RFC3339Nano, payload.Since)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, payload.Since)
		}
		if err != nil {
			return sendEvent(ctx, events, errorEvent("", "invalid_since", "since must be an RFC3339 timestamp"))
		}
		since = &parsed
	}

	page, err := h.chats.ListMessagesForUser(ctx, userID, chatID, domain.ListParams{Limit: h.pageLimit})
	if err != nil {
		slog.Error("web: websocket list messages", "error", err)
		return sendEvent(ctx, events, errorEvent("", "internal_error", "could not load history"))
	}

	messages, err := h.renderMessages(page.Messages)
	if err != nil {
		slog.Error("web: websocket render messages", "error", err)
		return sendEvent(ctx, events, errorEvent("", "internal_error", "could not render history"))
	}
	var nextCursor *string
	if page.HasMore && len(page.Messages) > 0 {
		encoded := cursor.Encode(page.Messages[0].Seq, cursor.Backward)
		nextCursor = &encoded
	}
	if !payload.Initial && since != nil {
		filtered := messages[:0]
		for _, message := range messages {
			if !message.CreatedAt.Before(*since) {
				filtered = append(filtered, message)
			}
		}
		messages = filtered
	}
	return sendEvent(ctx, events, realtime.Event{
		Type: "messages.page",
		Payload: realtime.MessagesPage{
			ChatID:     chatID,
			Messages:   messages,
			NextCursor: nextCursor,
		},
	})
}

func (h *Handler) handleBefore(
	ctx context.Context,
	userID, chatID int64,
	raw json.RawMessage,
	events chan<- realtime.Event,
) error {
	var payload beforePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return sendEvent(ctx, events, errorEvent("", "invalid_payload", "messages.before payload is invalid"))
	}
	if payload.ChatID != 0 && payload.ChatID != chatID {
		return sendEvent(ctx, events, errorEvent("", "invalid_chat", "chat_id does not match the connection"))
	}
	position, err := cursor.Decode(payload.Cursor)
	if err != nil || position.Direction != cursor.Backward {
		return sendEvent(ctx, events, errorEvent("", "invalid_cursor", "cursor is invalid"))
	}

	page, err := h.chats.ListMessagesForUser(ctx, userID, chatID, domain.ListParams{
		Limit:     h.pageLimit,
		BeforeSeq: &position.Seq,
	})
	if err != nil {
		slog.Error("web: websocket load older messages", "error", err)
		return sendEvent(ctx, events, errorEvent("", "internal_error", "could not load history"))
	}
	messages, err := h.renderMessages(page.Messages)
	if err != nil {
		slog.Error("web: websocket render older messages", "error", err)
		return sendEvent(ctx, events, errorEvent("", "internal_error", "could not render history"))
	}

	var nextCursor *string
	if page.HasMore && len(messages) > 0 {
		encoded := cursor.Encode(messages[0].Seq, cursor.Backward)
		nextCursor = &encoded
	}
	return sendEvent(ctx, events, realtime.Event{
		Type: "messages.page",
		Payload: realtime.MessagesPage{
			ChatID:     chatID,
			Messages:   messages,
			NextCursor: nextCursor,
		},
	})
}

func (h *Handler) handleSend(
	ctx context.Context,
	userID, chatID int64,
	raw json.RawMessage,
	events chan<- realtime.Event,
) error {
	var payload sendPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return sendEvent(ctx, events, errorEvent("", "invalid_payload", "message.send payload is invalid"))
	}
	clientMessageID := strings.TrimSpace(payload.ClientMessageID)
	if payload.ChatID != 0 && payload.ChatID != chatID {
		return sendEvent(ctx, events, errorEvent(clientMessageID, "invalid_chat", "chat_id does not match the connection"))
	}

	body := strings.TrimSpace(payload.Body)
	if body == "" {
		return sendEvent(ctx, events, errorEvent(clientMessageID, "empty_body", "body must not be empty"))
	}
	if len(body) > h.maxMessageBytes {
		return sendEvent(ctx, events, errorEvent(
			clientMessageID, "message_too_large",
			fmt.Sprintf("body must not exceed %d bytes", h.maxMessageBytes),
		))
	}
	if len(clientMessageID) > 128 {
		return sendEvent(ctx, events, errorEvent(
			clientMessageID, "invalid_client_message_id",
			"client_message_id must not exceed 128 bytes",
		))
	}

	var idempotencyKey *string
	if clientMessageID != "" {
		idempotencyKey = &clientMessageID
	}
	message, created, err := h.chats.CreateUserMessage(ctx, userID, chatID, body, idempotencyKey)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return sendEvent(ctx, events, errorEvent(clientMessageID, "chat_not_found", "chat no longer exists"))
		}
		slog.Error("web: websocket create message", "error", err)
		return sendEvent(ctx, events, errorEvent(clientMessageID, "internal_error", "could not send message"))
	}

	if err := sendEvent(ctx, events, realtime.Event{
		Type: "message.ack",
		Payload: realtime.MessageAck{
			ClientMessageID: clientMessageID,
			Seq:             message.Seq,
		},
	}); err != nil {
		return err
	}

	if !created {
		return nil
	}
	view, err := h.renderMessage(message)
	if err != nil {
		slog.Error("web: websocket render message", "error", err)
		return nil
	}
	event := realtime.Event{
		Type: "message.created",
		Payload: realtime.MessageCreated{
			ChatID:  chatID,
			Message: view,
		},
	}
	if h.hub != nil {
		h.hub.Publish(chatID, event)
	} else if err := sendEvent(ctx, events, event); err != nil {
		return err
	}

	if h.trimmer != nil {
		trims, err := h.trimmer.TrimAfterMessage(ctx, chatID)
		if err != nil {
			slog.Error("web: trim messages", "error", err)
			return nil
		}
		for _, trim := range trims {
			trimEvent := realtime.Event{
				Type: "history.trimmed",
				Payload: realtime.HistoryTrimmed{
					ChatID:     trim.ChatID,
					Deleted:    trim.Deleted,
					ThroughSeq: trim.ThroughSeq,
				},
			}
			if h.hub != nil {
				h.hub.Publish(trim.ChatID, trimEvent)
			} else if err := sendEvent(ctx, events, trimEvent); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *Handler) renderMessages(messages []domain.Message) ([]realtime.MessageView, error) {
	views := make([]realtime.MessageView, 0, len(messages))
	for _, message := range messages {
		view, err := h.renderMessage(message)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (h *Handler) renderMessage(message domain.Message) (realtime.MessageView, error) {
	html, err := h.renderer.Render(message.Body)
	if err != nil {
		return realtime.MessageView{}, err
	}
	return realtime.MessageView{
		Seq:       message.Seq,
		Author:    message.Author,
		Body:      message.Body,
		HTML:      html,
		CreatedAt: message.CreatedAt,
	}, nil
}

func sendEvent(ctx context.Context, events chan<- realtime.Event, event realtime.Event) error {
	select {
	case events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func errorEvent(clientMessageID, code, message string) realtime.Event {
	return realtime.Event{
		Type: "message.error",
		Payload: realtime.MessageError{
			ClientMessageID: clientMessageID,
			Code:            code,
			Message:         message,
		},
	}
}
