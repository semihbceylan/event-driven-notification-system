// Package notification holds the notification domain model, its validation
// rules, and its PostgreSQL storage.
package notification

import (
	"time"

	"github.com/google/uuid"
)

type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"
)

// Channels lists every supported channel, e.g. to start one worker pool each.
var Channels = []Channel{ChannelSMS, ChannelEmail, ChannelPush}

func (c Channel) Valid() bool {
	switch c {
	case ChannelSMS, ChannelEmail, ChannelPush:
		return true
	}
	return false
}

type Priority string

const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

func (p Priority) Valid() bool {
	switch p {
	case PriorityHigh, PriorityNormal, PriorityLow:
		return true
	}
	return false
}

// Rank is the value stored in the database. Lower ranks are processed first,
// so the worker can simply ORDER BY priority.
func (p Priority) Rank() int16 {
	switch p {
	case PriorityHigh:
		return 0
	case PriorityLow:
		return 2
	default:
		return 1
	}
}

func priorityFromRank(r int16) Priority {
	switch r {
	case 0:
		return PriorityHigh
	case 2:
		return PriorityLow
	default:
		return PriorityNormal
	}
}

type Status string

const (
	// StatusPending: waiting to be sent. This includes notifications scheduled
	// for later and ones waiting for a retry (attempts > 0).
	StatusPending Status = "pending"
	// StatusProcessing: claimed by a worker and being sent right now.
	StatusProcessing Status = "processing"
	// StatusSent: the provider accepted the message.
	StatusSent Status = "sent"
	// StatusFailed: a permanent error, or all retries are used up.
	StatusFailed Status = "failed"
	// StatusCancelled: cancelled by a client before it was sent.
	StatusCancelled Status = "cancelled"
)

func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusProcessing, StatusSent, StatusFailed, StatusCancelled:
		return true
	}
	return false
}

type Notification struct {
	ID                uuid.UUID  `json:"id"`
	BatchID           *uuid.UUID `json:"batch_id,omitempty"`
	IdempotencyKey    *string    `json:"idempotency_key,omitempty"`
	CorrelationID     *string    `json:"correlation_id,omitempty"`
	Channel           Channel    `json:"channel"`
	Recipient         string     `json:"recipient"`
	Content           string     `json:"content"`
	Priority          Priority   `json:"priority"`
	Status            Status     `json:"status"`
	Attempts          int        `json:"attempts"`
	MaxAttempts       int        `json:"max_attempts"`
	NextAttemptAt     time.Time  `json:"next_attempt_at"`
	ProviderMessageID *string    `json:"provider_message_id,omitempty"`
	LastError         *string    `json:"last_error,omitempty"`
	ScheduledAt       *time.Time `json:"scheduled_at,omitempty"`
	SentAt            *time.Time `json:"sent_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// New builds a pending notification from validated input.
//
// IDs are UUIDv7, which start with a timestamp. New rows therefore land at the
// end of the primary-key index instead of at random places in it, which keeps
// inserts fast as the table grows.
func New(in CreateInput, batchID *uuid.UUID, correlationID string, maxAttempts int, now time.Time) Notification {
	n := Notification{
		ID:            uuid.Must(uuid.NewV7()),
		BatchID:       batchID,
		Channel:       in.Channel,
		Recipient:     in.Recipient,
		Content:       in.Content,
		Priority:      in.Priority,
		Status:        StatusPending,
		MaxAttempts:   maxAttempts,
		NextAttemptAt: now,
		ScheduledAt:   in.ScheduledAt,
	}
	if in.IdempotencyKey != "" {
		key := in.IdempotencyKey
		n.IdempotencyKey = &key
	}
	if correlationID != "" {
		n.CorrelationID = &correlationID
	}
	if in.ScheduledAt != nil {
		n.NextAttemptAt = *in.ScheduledAt
	}
	return n
}
