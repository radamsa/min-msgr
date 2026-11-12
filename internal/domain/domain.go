package domain

import (
	"errors"
	"time"
)

var (
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
	ErrBlocked      = errors.New("user is blocked")
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrKeyLimit     = errors.New("key limit reached")
)

// AuthContext is the result of authenticating an agent API key.
type AuthContext struct {
	ChatID int64
	UserID int64
	Login  string
}

// Message is a single chat message as exposed by the API.
type Message struct {
	Seq       int64     `json:"seq"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// Chat is a chat owned by a user.
type Chat struct {
	ID         int64
	Label      string
	DaysLimit  int
	BytesLimit int64
}

// ListParams describes a page request. BeforeSeq is used for backward
// pagination; AfterSeq and Since are used for forward pagination.
type ListParams struct {
	Limit     int
	BeforeSeq *int64
	AfterSeq  *int64
	Since     *time.Time
}

// MessagePage is one page of messages in ascending seq order.
type MessagePage struct {
	Messages []Message
	HasMore  bool
}

// TrimResult describes messages removed by a storage limit. ThroughSeq is the
// highest deleted seq in the chat; clients may drop everything up to it.
type TrimResult struct {
	ChatID     int64
	Deleted    int64
	ThroughSeq int64
}
