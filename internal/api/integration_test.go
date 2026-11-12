package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dav/min-msgr/internal/api"
	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/store/postgres"
	"github.com/dav/min-msgr/internal/testdb"
)

func TestMessagesEndToEnd(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()

	ctx := context.Background()
	user, err := service.NewUserService(pool).Create(ctx, "e2e", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key, err := service.NewKeyService(pool).Create(ctx, user.ID, "E2E")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}

	handler := api.NewHandler(postgres.New(pool), api.Config{
		MaxMessageBytes: 1024,
		MaxAPILimit:     10,
		DefaultAPILimit: 5,
	})

	post := func(body, clientID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(
			http.MethodPost, "/api/v1/messages",
			strings.NewReader(`{"agent_name":"Agent","body":"`+body+`","client_message_id":"`+clientID+`"}`),
		)
		req.Header.Set("Authorization", "Bearer "+key.PlainKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post("  hello  ", "id-1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var created domain.Message
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatalf("decode POST response: %v", err)
	}
	if created.Author != "Agent@e2e" || created.Body != "hello" || created.Seq <= 0 {
		t.Errorf("created message = %+v", created)
	}

	rec = post("different", "id-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent POST status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var replayed domain.Message
	if err := json.NewDecoder(rec.Body).Decode(&replayed); err != nil {
		t.Fatalf("decode idempotent response: %v", err)
	}
	if replayed.Seq != created.Seq || replayed.Body != "hello" {
		t.Errorf("replayed message = %+v, want seq %d body hello", replayed, created.Seq)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/messages?limit=10", nil)
	req.Header.Set("Authorization", "Bearer "+key.PlainKey)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Messages []domain.Message `json:"messages"`
		Limit    int              `json:"limit"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(list.Messages) != 1 || list.Messages[0].Seq != created.Seq {
		t.Errorf("GET messages = %+v, want one message with seq %d", list.Messages, created.Seq)
	}
	if list.Limit != 10 {
		t.Errorf("GET limit = %d, want 10", list.Limit)
	}
}
