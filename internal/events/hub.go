package events

import (
	"sync"

	"github.com/google/uuid"
)

// Hub fans events out to the WebSocket clients of this API process.
type Hub struct {
	mu      sync.Mutex
	clients map[*Client]struct{}
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*Client]struct{})}
}

// Client is one WebSocket connection. Send has a buffer so a slow browser
// does not hold up the others; if it fills up, the client is dropped
// (Dropped is closed) and the browser reconnects and reloads.
type Client struct {
	Send    chan Event
	Dropped chan struct{}
	batchID *uuid.UUID // if set, only events of this batch
	once    sync.Once
}

func (h *Hub) Register(batchID *uuid.UUID) *Client {
	c := &Client{Send: make(chan Event, 256), Dropped: make(chan struct{}), batchID: batchID}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// Broadcast never blocks: delivering to a full buffer drops that client.
func (h *Hub) Broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c.batchID != nil && (ev.BatchID == nil || *ev.BatchID != *c.batchID) {
			continue
		}
		select {
		case c.Send <- ev:
		default:
			delete(h.clients, c)
			c.once.Do(func() { close(c.Dropped) })
		}
	}
}

// Len is the number of connected clients.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}
