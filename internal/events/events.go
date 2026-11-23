// Package events broadcasts notification status changes so that clients
// (the dashboard) can follow them in real time.
//
// The worker and the API publish to a Redis pub/sub channel; every API
// replica subscribes and forwards events to its WebSocket clients. Redis
// sits in the middle because the process that changes a status (a worker)
// is not the one holding the browser's connection (an API replica).
//
// Events are best-effort: pub/sub does not store messages, so a client that
// is disconnected misses them. That is fine for a live view. The database
// stays the source of truth, and the dashboard reloads from the API on
// reconnect.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

const redisChannel = "notification-events"

// Event is one status change. It carries enough for a list view to update
// a row without fetching the notification again.
type Event struct {
	ID        uuid.UUID             `json:"id"`
	BatchID   *uuid.UUID            `json:"batch_id,omitempty"`
	Channel   notification.Channel  `json:"channel"`
	Priority  notification.Priority `json:"priority"`
	Recipient string                `json:"recipient"`
	Status    notification.Status   `json:"status"`
	Attempts  int                   `json:"attempts"`
	LastError *string               `json:"last_error,omitempty"`
	At        time.Time             `json:"at"`
}

// FromNotification builds an event from a notification in its new state.
func FromNotification(n notification.Notification, at time.Time) Event {
	return Event{
		ID: n.ID, BatchID: n.BatchID, Channel: n.Channel, Priority: n.Priority, Recipient: n.Recipient,
		Status: n.Status, Attempts: n.Attempts, LastError: n.LastError, At: at.UTC(),
	}
}

type Publisher struct {
	rdb    *redis.Client
	logger *slog.Logger
}

func NewPublisher(rdb *redis.Client, logger *slog.Logger) *Publisher {
	return &Publisher{rdb: rdb, logger: logger}
}

// Publish sends events in one round trip. Failures are logged, not
// returned: a broken live view must never block or fail a delivery.
func (p *Publisher) Publish(ctx context.Context, evs ...Event) {
	if p == nil || len(evs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	pipe := p.rdb.Pipeline()
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		pipe.Publish(ctx, redisChannel, b)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		p.logger.Warn("could not publish status events", "count", len(evs), "error", err)
	}
}

// Subscribe calls fn for every event until ctx is cancelled, reconnecting
// on errors (go-redis resubscribes automatically).
func Subscribe(ctx context.Context, rdb *redis.Client, logger *slog.Logger, fn func(Event)) {
	sub := rdb.Subscribe(ctx, redisChannel)
	defer func() { _ = sub.Close() }()

	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var ev Event
			if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
				logger.Warn("dropping malformed status event", "error", err)
				continue
			}
			fn(ev)
		}
	}
}
