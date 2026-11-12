// Package realtime broadcasts chat events to connected web clients.
//
// A Hub keeps one subscription per WebSocket connection. Publishing never
// blocks: a slow client misses events instead of stalling the chat. Clients
// recover missed state by reloading history after a reconnect.
package realtime

import (
	"log/slog"
	"sync"
	"time"
)

// Event is one server-to-client WebSocket event.
type Event struct {
	Type    string
	Payload any
}

// MessageView is a message as rendered for the web client. Body is the original
// CommonMark; HTML is the sanitized rendering.
type MessageView struct {
	Seq       int64     `json:"seq"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	HTML      string    `json:"html"`
	CreatedAt time.Time `json:"created_at"`
}

// MessageCreated is the payload of a message.created event.
type MessageCreated struct {
	ChatID  int64       `json:"chat_id"`
	Message MessageView `json:"message"`
}

// MessagesPage is the payload of a messages.page event.
type MessagesPage struct {
	ChatID     int64         `json:"chat_id"`
	Messages   []MessageView `json:"messages"`
	NextCursor *string       `json:"next_cursor"`
}

// MessageAck acknowledges a message.send request.
type MessageAck struct {
	ClientMessageID string `json:"client_message_id"`
	Seq             int64  `json:"seq"`
}

// MessageError reports a rejected message.send request.
type MessageError struct {
	ClientMessageID string `json:"client_message_id,omitempty"`
	Code            string `json:"code"`
	Message         string `json:"message"`
}

// HistoryTrimmed reports that old messages were deleted by a storage limit.
type HistoryTrimmed struct {
	ChatID     int64 `json:"chat_id"`
	Deleted    int64 `json:"deleted"`
	ThroughSeq int64 `json:"through_seq"`
}

// ChatDeleted reports that the chat and its key were revoked.
type ChatDeleted struct {
	ChatID int64 `json:"chat_id"`
}

// Subscription receives events for one chat. Close must be called when the
// connection ends.
type Subscription struct {
	hub    *Hub
	chatID int64
	events chan Event
	once   sync.Once
}

// Events returns the subscription channel.
func (s *Subscription) Events() <-chan Event {
	return s.events
}

// Close removes the subscription from its hub.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.hub.remove(s.chatID, s)
		close(s.events)
	})
}

// Hub fan-outs events by chat ID.
type Hub struct {
	mu   sync.RWMutex
	subs map[int64]map[*Subscription]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: make(map[int64]map[*Subscription]struct{})}
}

// Subscribe registers a new listener for a chat.
func (h *Hub) Subscribe(chatID int64) *Subscription {
	s := &Subscription{
		hub:    h,
		chatID: chatID,
		events: make(chan Event, 64),
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[chatID] == nil {
		h.subs[chatID] = make(map[*Subscription]struct{})
	}
	h.subs[chatID][s] = struct{}{}
	return s
}

// Publish delivers an event to every current subscriber of the chat. The call
// is non-blocking; subscribers whose buffers are full skip the event.
func (h *Hub) Publish(chatID int64, event Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs[chatID] {
		select {
		case s.events <- event:
		default:
			slog.Warn("realtime: subscriber buffer full, dropping event",
				"chat_id", chatID, "event", event.Type)
		}
	}
}

func (h *Hub) remove(chatID int64, target *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	subs := h.subs[chatID]
	delete(subs, target)
	if len(subs) == 0 {
		delete(h.subs, chatID)
	}
}
