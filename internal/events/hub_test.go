package events

import (
	"testing"

	"github.com/google/uuid"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

func TestHubFiltersByBatch(t *testing.T) {
	h := NewHub()
	batch := uuid.New()
	all := h.Register(nil)
	onlyBatch := h.Register(&batch)

	h.Broadcast(Event{ID: uuid.New(), Status: notification.StatusSent})                  // no batch
	h.Broadcast(Event{ID: uuid.New(), BatchID: &batch, Status: notification.StatusSent}) // this batch

	if len(all.Send) != 2 {
		t.Errorf("unfiltered client got %d events, want 2", len(all.Send))
	}
	if len(onlyBatch.Send) != 1 {
		t.Errorf("batch client got %d events, want 1", len(onlyBatch.Send))
	}
}

func TestHubDropsSlowClient(t *testing.T) {
	h := NewHub()
	slow := h.Register(nil)
	fast := h.Register(nil)

	for range cap(slow.Send) + 1 {
		h.Broadcast(Event{ID: uuid.New()})
		// The fast client keeps up.
		select {
		case <-fast.Send:
		default:
		}
	}

	select {
	case <-slow.Dropped:
	default:
		t.Fatal("slow client should be dropped when its buffer is full")
	}
	if h.Len() != 1 {
		t.Errorf("hub has %d clients, want 1 (the fast one)", h.Len())
	}

	// Broadcasting more must not panic on the dropped client.
	h.Broadcast(Event{ID: uuid.New()})
}
