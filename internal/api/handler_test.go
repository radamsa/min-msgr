package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dav/min-msgr/internal/domain"
	"github.com/dav/min-msgr/internal/store"
)

type fakeStore struct {
	auth      domain.AuthContext
	authErr   error
	message   domain.Message
	created   bool
	createErr error

	createChatID   int64
	createAgent    string
	createBody     string
	createClientID *string

	page       domain.MessagePage
	listErr    error
	listChatID int64
	listParams domain.ListParams
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		auth: domain.AuthContext{ChatID: 1, UserID: 2, Login: "ivan"},
	}
}

func (f *fakeStore) AuthenticateAPIKey(context.Context, []byte) (domain.AuthContext, error) {
	return f.auth, f.authErr
}

func (f *fakeStore) CreateAgentMessage(
	_ context.Context,
	chatID int64,
	agentName string,
	body string,
	clientMessageID *string,
) (domain.Message, bool, error) {
	f.createChatID = chatID
	f.createAgent = agentName
	f.createBody = body
	f.createClientID = clientMessageID
	return f.message, f.created, f.createErr
}

func (f *fakeStore) ListMessages(
	_ context.Context,
	chatID int64,
	params domain.ListParams,
) (domain.MessagePage, error) {
	f.listChatID = chatID
	f.listParams = params
	return f.page, f.listErr
}

var _ store.Store = (*fakeStore)(nil)

func testHandler(f *fakeStore) http.Handler {
	return NewHandler(f, Config{
		MaxMessageBytes: 10,
		MaxAPILimit:     5,
		DefaultAPILimit: 3,
	})
}

func performRequest(t *testing.T, handler http.Handler, method, target, body, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestPostMessageRequiresAuth(t *testing.T) {
	f := newFakeStore()
	rec := performRequest(t, testHandler(f), http.MethodPost, "/api/v1/messages", `{}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestPostMessageRejectsBadAuth(t *testing.T) {
	tests := []string{"Basic abc", "Bearer", "Bearer not-a-key"}
	for _, auth := range tests {
		f := newFakeStore()
		rec := performRequest(t, testHandler(f), http.MethodPost, "/api/v1/messages", `{}`, auth)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: status = %d, want 401", auth, rec.Code)
		}
	}
}

func TestPostMessageCreates(t *testing.T) {
	f := newFakeStore()
	f.created = true
	f.message = domain.Message{
		Seq:       7,
		Author:    "agent@ivan",
		Body:      "hello",
		CreatedAt: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC),
	}
	rec := performRequest(
		t, testHandler(f), http.MethodPost, "/api/v1/messages",
		`{"agent_name":"agent","body":"  hello  ","client_message_id":" id-1 "}`,
		"Bearer sk_test",
	)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var got domain.Message
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got != f.message {
		t.Errorf("message = %+v, want %+v", got, f.message)
	}
	if f.createChatID != 1 || f.createAgent != "agent" || f.createBody != "hello" {
		t.Errorf("store call = chat %d agent %q body %q", f.createChatID, f.createAgent, f.createBody)
	}
	if f.createClientID == nil || *f.createClientID != "id-1" {
		t.Errorf("client message id = %v, want id-1", f.createClientID)
	}
}

func TestPostMessageIdempotent(t *testing.T) {
	f := newFakeStore()
	f.created = false
	f.message = domain.Message{Seq: 7, Author: "agent@ivan", Body: "hello"}
	rec := performRequest(
		t, testHandler(f), http.MethodPost, "/api/v1/messages",
		`{"agent_name":"agent","body":"hello","client_message_id":"id-1"}`,
		"Bearer sk_test",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestPostMessageValidation(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "invalid json", body: `{`, code: "invalid_json"},
		{name: "trailing json", body: `{"agent_name":"a","body":"x"}{}`, code: "invalid_json"},
		{name: "empty agent", body: `{"agent_name":"","body":"x"}`, code: "invalid_agent_name"},
		{name: "bad agent", body: `{"agent_name":"bad name","body":"x"}`, code: "invalid_agent_name"},
		{name: "long agent", body: `{"agent_name":"abcdefghijklmnopq","body":"x"}`, code: "invalid_agent_name"},
		{name: "empty body", body: `{"agent_name":"agent","body":"   "}`, code: "empty_body"},
		{name: "long body", body: `{"agent_name":"agent","body":"12345678901"}`, code: "message_too_large"},
		{name: "long client id", body: `{"agent_name":"agent","body":"x","client_message_id":"` + strings.Repeat("a", 129) + `"}`, code: "invalid_client_message_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeStore()
			rec := performRequest(
				t, testHandler(f), http.MethodPost, "/api/v1/messages", tt.body,
				"Bearer sk_test",
			)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			var resp errorResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if resp.Error.Code != tt.code {
				t.Errorf("error code = %q, want %q", resp.Error.Code, tt.code)
			}
		})
	}
}

func TestPostMessageStoreUnauthorized(t *testing.T) {
	f := newFakeStore()
	f.createErr = domain.ErrUnauthorized
	rec := performRequest(
		t, testHandler(f), http.MethodPost, "/api/v1/messages",
		`{"agent_name":"agent","body":"hello"}`, "Bearer sk_test",
	)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestListMessagesDefaultLimit(t *testing.T) {
	f := newFakeStore()
	f.page = domain.MessagePage{Messages: []domain.Message{{Seq: 1}}}
	rec := performRequest(t, testHandler(f), http.MethodGet, "/api/v1/messages", "", "Bearer sk_test")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if f.listChatID != 1 || f.listParams.Limit != 3 {
		t.Errorf("store call = chat %d limit %d, want chat 1 limit 3", f.listChatID, f.listParams.Limit)
	}
	var resp listMessagesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Limit != 3 {
		t.Errorf("response limit = %d, want 3", resp.Limit)
	}
}

func TestListMessagesClampsLimit(t *testing.T) {
	f := newFakeStore()
	rec := performRequest(t, testHandler(f), http.MethodGet, "/api/v1/messages?limit=99", "", "Bearer sk_test")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if f.listParams.Limit != 5 {
		t.Errorf("store limit = %d, want 5", f.listParams.Limit)
	}
}

func TestListMessagesValidation(t *testing.T) {
	tests := []struct {
		name string
		url  string
		code string
	}{
		{name: "zero limit", url: "/api/v1/messages?limit=0", code: "invalid_limit"},
		{name: "negative limit", url: "/api/v1/messages?limit=-1", code: "invalid_limit"},
		{name: "bad limit", url: "/api/v1/messages?limit=abc", code: "invalid_limit"},
		{name: "bad since", url: "/api/v1/messages?since=yesterday", code: "invalid_since"},
		{name: "bad cursor", url: "/api/v1/messages?cursor=!!!", code: "invalid_cursor"},
		{name: "forward cursor without since", url: "/api/v1/messages?cursor=" + encodeCursor(1, cursorForward), code: "invalid_cursor"},
		{name: "backward cursor with since", url: "/api/v1/messages?since=2026-01-01T00:00:00Z&cursor=" + encodeCursor(1, cursorBackward), code: "invalid_cursor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeStore()
			rec := performRequest(t, testHandler(f), http.MethodGet, tt.url, "", "Bearer sk_test")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}
			var resp errorResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if resp.Error.Code != tt.code {
				t.Errorf("error code = %q, want %q", resp.Error.Code, tt.code)
			}
		})
	}
}

func TestListMessagesBackwardCursor(t *testing.T) {
	f := newFakeStore()
	f.page = domain.MessagePage{
		Messages: []domain.Message{{Seq: 4}, {Seq: 5}},
		HasMore:  true,
	}
	rec := performRequest(
		t, testHandler(f), http.MethodGet,
		"/api/v1/messages?cursor="+encodeCursor(10, cursorBackward), "", "Bearer sk_test",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if f.listParams.BeforeSeq == nil || *f.listParams.BeforeSeq != 10 || f.listParams.Since != nil {
		t.Errorf("list params = %+v, want BeforeSeq=10", f.listParams)
	}
	var resp listMessagesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.NextCursor == nil {
		t.Fatal("next_cursor = nil, want backward cursor")
	}
	next, err := decodeCursor(*resp.NextCursor)
	if err != nil {
		t.Fatalf("decode next cursor: %v", err)
	}
	if next.Seq != 4 || next.Direction != cursorBackward {
		t.Errorf("next cursor = %+v, want seq 4 backward", next)
	}
}

func TestListMessagesForwardCursor(t *testing.T) {
	f := newFakeStore()
	f.page = domain.MessagePage{
		Messages: []domain.Message{{Seq: 4}, {Seq: 5}},
		HasMore:  true,
	}
	rec := performRequest(
		t, testHandler(f), http.MethodGet,
		"/api/v1/messages?since=2026-01-01T00:00:00Z&cursor="+encodeCursor(3, cursorForward),
		"", "Bearer sk_test",
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if f.listParams.AfterSeq == nil || *f.listParams.AfterSeq != 3 || f.listParams.Since == nil {
		t.Errorf("list params = %+v, want AfterSeq=3 and Since set", f.listParams)
	}
	var resp listMessagesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.NextCursor == nil {
		t.Fatal("next_cursor = nil, want forward cursor")
	}
	next, err := decodeCursor(*resp.NextCursor)
	if err != nil {
		t.Fatalf("decode next cursor: %v", err)
	}
	if next.Seq != 5 || next.Direction != cursorForward {
		t.Errorf("next cursor = %+v, want seq 5 forward", next)
	}
}

func TestListMessagesNoMore(t *testing.T) {
	f := newFakeStore()
	f.page = domain.MessagePage{Messages: []domain.Message{{Seq: 1}}}
	rec := performRequest(t, testHandler(f), http.MethodGet, "/api/v1/messages", "", "Bearer sk_test")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp listMessagesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.NextCursor != nil {
		t.Errorf("next_cursor = %q, want nil", *resp.NextCursor)
	}
}

func TestListMessagesEmptyIsArray(t *testing.T) {
	f := newFakeStore()
	rec := performRequest(t, testHandler(f), http.MethodGet, "/api/v1/messages", "", "Bearer sk_test")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"messages":[]`)) {
		t.Errorf("empty messages should be an array, got %s", rec.Body.String())
	}
}

func TestListMessagesStoreError(t *testing.T) {
	f := newFakeStore()
	f.listErr = errors.New("boom")
	rec := performRequest(t, testHandler(f), http.MethodGet, "/api/v1/messages", "", "Bearer sk_test")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
