package notification

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Content limits per channel, counted in characters (runes), not bytes, so
// Turkish or emoji text is not penalised.
const (
	// An SMS segment holds 160 characters; providers join up to about 10
	// segments into one long message.
	MaxSMSLength = 1600
	// Push payloads are capped at 4 KB by APNs/FCM, including metadata.
	MaxPushLength = 1000
	// Generous, but keeps a 1000-item batch request at a sane size.
	MaxEmailLength = 20000

	MaxPushTokenLength      = 512
	MaxIdempotencyKeyLength = 255
	MaxBatchSize            = 1000
	MaxScheduleAhead        = 365 * 24 * time.Hour
)

// E.164: a plus sign followed by up to 15 digits, with no leading zero.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// CreateInput is the body of a create request, and one item of a batch.
type CreateInput struct {
	Recipient      string     `json:"recipient"`
	Channel        Channel    `json:"channel"`
	Content        string     `json:"content"`
	Priority       Priority   `json:"priority,omitempty"`
	ScheduledAt    *time.Time `json:"scheduled_at,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
}

// FieldError describes one invalid field. Validation returns all of them at
// once, so a client can fix every problem in one go.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Normalize trims whitespace and applies defaults. Call it before Validate.
func (in *CreateInput) Normalize() {
	in.Recipient = strings.TrimSpace(in.Recipient)
	in.Channel = Channel(strings.ToLower(strings.TrimSpace(string(in.Channel))))
	in.Priority = Priority(strings.ToLower(strings.TrimSpace(string(in.Priority))))
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.Priority == "" {
		in.Priority = PriorityNormal
	}
}

// Validate returns every field error in the input; nil means it is valid.
// prefix is prepended to field names, e.g. "notifications[3]." in a batch.
func (in CreateInput) Validate(prefix string, now time.Time) []FieldError {
	var errs []FieldError
	add := func(field, msg string) {
		errs = append(errs, FieldError{Field: prefix + field, Message: msg})
	}

	if !in.Channel.Valid() {
		add("channel", "must be one of: sms, email, push")
	}
	if !in.Priority.Valid() {
		add("priority", "must be one of: high, normal, low")
	}

	if in.Recipient == "" {
		add("recipient", "is required")
	} else {
		switch in.Channel {
		case ChannelSMS:
			if !e164.MatchString(in.Recipient) {
				add("recipient", "must be a phone number in E.164 format, e.g. +905551234567")
			}
		case ChannelEmail:
			// ParseAddress also accepts `Name <a@b.com>`; we only want the bare address.
			addr, err := mail.ParseAddress(in.Recipient)
			if err != nil || addr.Address != in.Recipient {
				add("recipient", "must be a valid email address")
			}
		case ChannelPush:
			if utf8.RuneCountInString(in.Recipient) > MaxPushTokenLength || strings.IndexFunc(in.Recipient, unicode.IsSpace) >= 0 {
				add("recipient", fmt.Sprintf("must be a device token of at most %d characters without spaces", MaxPushTokenLength))
			}
		}
	}

	if strings.TrimSpace(in.Content) == "" {
		add("content", "is required")
	} else if limit, ok := contentLimit(in.Channel); ok {
		if n := utf8.RuneCountInString(in.Content); n > limit {
			add("content", fmt.Sprintf("is %d characters; the limit for %s is %d", n, in.Channel, limit))
		}
	}

	if in.ScheduledAt != nil {
		switch {
		case in.ScheduledAt.Before(now):
			add("scheduled_at", "must be in the future")
		case in.ScheduledAt.After(now.Add(MaxScheduleAhead)):
			add("scheduled_at", "must be within one year")
		}
	}

	if utf8.RuneCountInString(in.IdempotencyKey) > MaxIdempotencyKeyLength {
		add("idempotency_key", fmt.Sprintf("must be at most %d characters", MaxIdempotencyKeyLength))
	}
	return errs
}

// ValidateBatch validates every item and the batch as a whole. A batch is
// accepted or rejected as one unit, which is simpler for clients than
// partial success.
func ValidateBatch(items []CreateInput, now time.Time) []FieldError {
	if len(items) == 0 {
		return []FieldError{{Field: "notifications", Message: "must contain at least one notification"}}
	}
	if len(items) > MaxBatchSize {
		return []FieldError{{Field: "notifications", Message: fmt.Sprintf("must contain at most %d notifications, got %d", MaxBatchSize, len(items))}}
	}

	var errs []FieldError
	seenKeys := make(map[string]int)
	for i, item := range items {
		prefix := fmt.Sprintf("notifications[%d].", i)
		errs = append(errs, item.Validate(prefix, now)...)

		if item.IdempotencyKey == "" {
			continue
		}
		if first, dup := seenKeys[item.IdempotencyKey]; dup {
			errs = append(errs, FieldError{
				Field:   prefix + "idempotency_key",
				Message: fmt.Sprintf("duplicates the key of notifications[%d]", first),
			})
			continue
		}
		seenKeys[item.IdempotencyKey] = i
	}
	return errs
}

func contentLimit(c Channel) (int, bool) {
	switch c {
	case ChannelSMS:
		return MaxSMSLength, true
	case ChannelEmail:
		return MaxEmailLength, true
	case ChannelPush:
		return MaxPushLength, true
	}
	return 0, false
}
