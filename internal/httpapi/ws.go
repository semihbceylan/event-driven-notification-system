package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/semihbceylan/event-driven-notification-system/internal/events"
	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
)

// WSHandler streams notification status changes over WebSocket.
type WSHandler struct {
	Hub *events.Hub
	// AllowedOrigins are extra browser origins allowed to connect (the
	// dashboard dev server). Same-origin requests are always allowed.
	AllowedOrigins []string
}

// Serve handles GET /v1/ws, optionally ?batch_id= to follow one batch.
func (h WSHandler) Serve(w http.ResponseWriter, r *http.Request) {
	var batchID *uuid.UUID
	if v := r.URL.Query().Get("batch_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_id", "batch_id must be a UUID", nil)
			return
		}
		batchID = &id
	}

	// The server's read and write timeouts are meant for normal requests.
	// A WebSocket stays open for as long as the dashboard is open, so
	// clear them for this connection.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: h.AllowedOrigins})
	if err != nil {
		return // Accept has already written the error response
	}
	// Closing an already-closed connection returns an error we cannot act on.
	defer func() { _ = conn.CloseNow() }()

	log := logging.FromContext(r.Context())
	client := h.Hub.Register(batchID)
	defer h.Hub.Unregister(client)
	log.Info("websocket client connected", "batch_id", batchID, "clients", h.Hub.Len())

	// We never expect messages from the client; CloseRead handles control
	// frames and cancels ctx when the browser goes away.
	ctx := conn.CloseRead(r.Context())
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-client.Dropped:
			conn.Close(websocket.StatusPolicyViolation, "too slow; reconnect and reload")
			return
		case <-ping.C:
			// Keeps proxies from closing an idle connection.
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case ev := <-client.Send:
			wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := wsjson.Write(wctx, conn, ev)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
