package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/semihbceylan/event-driven-notification-system/internal/events"
	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

// The WebSocket goes through the same middleware as every other request,
// so this also checks that the logging wrapper supports the upgrade.
func TestWebSocketStreamsEvents(t *testing.T) {
	hub := events.NewHub()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewRouter(Deps{
		Logger:        logger,
		Notifications: &NotificationHandler{Repo: newFakeRepo()},
		WS:            &WSHandler{Hub: hub},
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	batch := uuid.New()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/ws?batch_id=" + batch.String()
	conn, resp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	// Wait until the server has registered the client.
	for hub.Len() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("client never registered")
		case <-time.After(5 * time.Millisecond):
		}
	}

	other := uuid.New()
	hub.Broadcast(events.Event{ID: uuid.New(), BatchID: &other, Status: notification.StatusSent}) // filtered out
	want := events.Event{ID: uuid.New(), BatchID: &batch, Channel: notification.ChannelSMS, Status: notification.StatusSent, Attempts: 1}
	hub.Broadcast(want)

	var got events.Event
	if err := wsjson.Read(ctx, conn, &got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.ID != want.ID || got.Status != notification.StatusSent || got.Attempts != 1 {
		t.Errorf("got %+v, want %+v", got, want)
	}

	conn.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for hub.Len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hub.Len() != 0 {
		t.Error("client not unregistered after disconnect")
	}
}

func TestWebSocketRejectsBadBatchID(t *testing.T) {
	srv := httptest.NewServer(NewRouter(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notifications: &NotificationHandler{Repo: newFakeRepo()},
		WS:            &WSHandler{Hub: events.NewHub()},
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/ws?batch_id=nope")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
