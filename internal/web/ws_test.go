package web_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dav/min-msgr/internal/api"
	"github.com/dav/min-msgr/internal/realtime"
	"github.com/dav/min-msgr/internal/service"
	"github.com/dav/min-msgr/internal/store/postgres"
	"github.com/dav/min-msgr/internal/testdb"
	"github.com/dav/min-msgr/internal/web"
)

type wsEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func TestWebSocketChatFlow(t *testing.T) {
	pool, cleanup := testdb.New(t)
	defer cleanup()
	ctx := context.Background()

	user, err := service.NewUserService(pool).Create(ctx, "ws-user", "")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	key, err := service.NewKeyService(pool).Create(ctx, user.ID, "WS Chat")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	sessionSvc := service.NewSessionService(pool, time.Hour)
	token, _, err := sessionSvc.Create(ctx, user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	hub := realtime.NewHub()
	archiveDir := t.TempDir()
	webHandler, err := web.NewHandler(web.Options{
		OAuth:    service.NewOAuthService(pool),
		Sessions: sessionSvc,
		Keys:     service.NewKeyService(pool),
		Archives: service.NewArchiveService(pool, archiveDir),
		Chats:    postgres.New(pool),
		Trimmer:  service.NewLimitsService(pool),
		Hub:      hub,
		Config:   web.Config{MaxMessageBytes: 1024, PageLimit: 2},
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	root := http.NewServeMux()
	root.Handle("/api/", api.NewHandler(postgres.New(pool), api.Config{
		MaxMessageBytes: 1024,
		MaxAPILimit:     100,
		DefaultAPILimit: 50,
		Notifier:        hub,
	}))
	root.Handle("/", webHandler.Routes())
	server, client := newTestServer(t, root)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	client.Jar.SetCookies(serverURL, []*http.Cookie{{
		Name: "minmsgr_session", Value: token, Path: "/",
	}})

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") +
		"/ws?chat_id=" + strconv.FormatInt(key.ChatID, 10)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": {"minmsgr_session=" + token}},
	})
	if err != nil {
		t.Fatalf("websocket dial: %v", err)
	}
	defer conn.CloseNow()

	writeWS(t, ctx, conn, "messages.subscribe", map[string]any{
		"chat_id": key.ChatID,
		"initial": true,
	})
	event := readWS(t, ctx, conn)
	if event.Type != "messages.page" {
		t.Fatalf("first event = %s, want messages.page", event.Type)
	}
	var page realtime.MessagesPage
	decodePayload(t, event, &page)
	if len(page.Messages) != 0 || page.NextCursor != nil {
		t.Errorf("initial page = %+v, want empty", page)
	}

	writeWS(t, ctx, conn, "message.send", map[string]any{
		"chat_id":           key.ChatID,
		"body":              "**hello**",
		"client_message_id": "m1",
	})
	ack, created := readAckAndCreated(t, ctx, conn)
	if ack.ClientMessageID != "m1" || ack.Seq == 0 {
		t.Errorf("ack = %+v", ack)
	}
	if created.Message.Seq != ack.Seq {
		t.Errorf("created seq = %d, ack seq = %d", created.Message.Seq, ack.Seq)
	}
	if created.Message.Author != user.Login {
		t.Errorf("created author = %q, want %q", created.Message.Author, user.Login)
	}
	if !strings.Contains(created.Message.HTML, "<strong>hello</strong>") {
		t.Errorf("created html = %q", created.Message.HTML)
	}

	// A retry with the same client_message_id is idempotent: an ack, no
	// second message.created event.
	writeWS(t, ctx, conn, "message.send", map[string]any{
		"chat_id":           key.ChatID,
		"body":              "**hello**",
		"client_message_id": "m1",
	})
	duplicateAck := readWS(t, ctx, conn)
	if duplicateAck.Type != "message.ack" {
		t.Fatalf("duplicate event = %s, want message.ack", duplicateAck.Type)
	}
	var messageCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE chat_id = $1`, key.ChatID).
		Scan(&messageCount); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messageCount != 1 {
		t.Fatalf("messages after duplicate send = %d, want 1", messageCount)
	}

	// Fill the chat so the page limit forces a cursor.
	for _, body := range []string{"second", "third"} {
		writeWS(t, ctx, conn, "message.send", map[string]any{
			"chat_id": key.ChatID,
			"body":    body,
		})
		readAckAndCreated(t, ctx, conn)
	}

	writeWS(t, ctx, conn, "messages.subscribe", map[string]any{
		"chat_id": key.ChatID,
		"initial": true,
	})
	event = readWS(t, ctx, conn)
	if event.Type != "messages.page" {
		t.Fatalf("subscribe event = %s, want messages.page", event.Type)
	}
	page = realtime.MessagesPage{}
	decodePayload(t, event, &page)
	if len(page.Messages) != 2 || page.NextCursor == nil {
		t.Fatalf("page = %+v, want 2 messages and a cursor", page)
	}
	if page.Messages[1].Body != "third" {
		t.Errorf("last message = %q, want third", page.Messages[1].Body)
	}

	writeWS(t, ctx, conn, "messages.before", map[string]any{
		"chat_id": key.ChatID,
		"cursor":  *page.NextCursor,
	})
	event = readWS(t, ctx, conn)
	if event.Type != "messages.page" {
		t.Fatalf("before event = %s, want messages.page", event.Type)
	}
	older := realtime.MessagesPage{}
	decodePayload(t, event, &older)
	if len(older.Messages) != 1 || older.Messages[0].Body != "**hello**" || older.NextCursor != nil {
		t.Errorf("older page = %+v", older)
	}

	// An agent message posted through the REST API reaches the WebSocket.
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/messages",
		strings.NewReader(`{"agent_name":"Bot","body":"from rest"}`))
	if err != nil {
		t.Fatalf("create REST request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key.PlainKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST agent message: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("agent message status = %d, want 201", resp.StatusCode)
	}
	event = readWS(t, ctx, conn)
	if event.Type != "message.created" {
		t.Fatalf("REST event = %s, want message.created", event.Type)
	}
	var restCreated realtime.MessageCreated
	decodePayload(t, event, &restCreated)
	if restCreated.Message.Author != "Bot@"+user.Login || restCreated.Message.Body != "from rest" {
		t.Errorf("REST created = %+v", restCreated)
	}

	// Revoking the key archives the chat and notifies the open connection.
	revokeReq, err := http.NewRequest(http.MethodPost,
		server.URL+"/keys/"+strconv.FormatInt(key.ChatID, 10)+"/revoke", nil)
	if err != nil {
		t.Fatalf("create revoke request: %v", err)
	}
	resp, err = client.Do(revokeReq)
	if err != nil {
		t.Fatalf("POST revoke: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke status = %d, want 303", resp.StatusCode)
	}
	event = readWS(t, ctx, conn)
	if event.Type != "chat.deleted" {
		t.Fatalf("revoke event = %s, want chat.deleted", event.Type)
	}
	var deleted realtime.ChatDeleted
	decodePayload(t, event, &deleted)
	if deleted.ChatID != key.ChatID {
		t.Errorf("deleted chat = %d, want %d", deleted.ChatID, key.ChatID)
	}
}

func TestWebSocketRejectsUnauthorized(t *testing.T) {
	handler, err := web.NewHandler(web.Options{
		OAuth: fakeOAuth{}, Sessions: fakeSessions{}, Keys: fakeKeys{},
		Archives: fakeArchives{}, Chats: fakeChats{},
		Hub: realtime.NewHub(),
	})
	if err != nil {
		t.Fatalf("NewHandler() error = %v", err)
	}
	server := httptest.NewServer(handler.Routes())
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?chat_id=1"
	conn, resp, err := websocket.Dial(context.Background(), wsURL, nil)
	if conn != nil {
		conn.CloseNow()
	}
	if err == nil {
		t.Fatal("websocket dial succeeded without a session")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("dial response = %+v, want 401", resp)
	}
}

func writeWS(t *testing.T, ctx context.Context, conn *websocket.Conn, eventType string, payload any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": eventType, "payload": payload})
	if err != nil {
		t.Fatalf("marshal %s: %v", eventType, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write %s: %v", eventType, err)
	}
}

func readWS(t *testing.T, ctx context.Context, conn *websocket.Conn) wsEvent {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		t.Fatalf("read websocket event: %v", err)
	}
	var event wsEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode websocket event %q: %v", data, err)
	}
	return event
}

func decodePayload(t *testing.T, event wsEvent, target any) {
	t.Helper()
	if err := json.Unmarshal(event.Payload, target); err != nil {
		t.Fatalf("decode %s payload: %v", event.Type, err)
	}
}

func readAckAndCreated(t *testing.T, ctx context.Context, conn *websocket.Conn) (realtime.MessageAck, realtime.MessageCreated) {
	t.Helper()
	var ack realtime.MessageAck
	var created realtime.MessageCreated
	for i := 0; i < 2; i++ {
		event := readWS(t, ctx, conn)
		switch event.Type {
		case "message.ack":
			decodePayload(t, event, &ack)
		case "message.created":
			decodePayload(t, event, &created)
		default:
			t.Fatalf("unexpected event %s while sending", event.Type)
		}
	}
	if ack.Seq == 0 || created.Message.Seq == 0 {
		t.Fatalf("missing ack or created event: ack=%+v created=%+v", ack, created)
	}
	return ack, created
}
