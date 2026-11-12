package store

import (
	"context"

	"github.com/dav/min-msgr/internal/domain"
)

// Store is the persistence contract used by the HTTP API.
type Store interface {
	AuthenticateAPIKey(ctx context.Context, keyHash []byte) (domain.AuthContext, error)
	CreateAgentMessage(
		ctx context.Context,
		chatID int64,
		agentName string,
		body string,
		clientMessageID *string,
	) (message domain.Message, created bool, err error)
	ListMessages(ctx context.Context, chatID int64, params domain.ListParams) (domain.MessagePage, error)
}
