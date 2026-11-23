package notification

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func valid(ch Channel) CreateInput {
	in := CreateInput{Channel: ch, Content: "Hello", Priority: PriorityNormal}
	switch ch {
	case ChannelSMS:
		in.Recipient = "+905551234567"
	case ChannelEmail:
		in.Recipient = "user@example.com"
	case ChannelPush:
		in.Recipient = "device-token-abc123"
	}
	return in
}

func TestValidate(t *testing.T) {
	future := now.Add(time.Hour)
	past := now.Add(-time.Minute)
	tooFar := now.Add(MaxScheduleAhead + time.Hour)

	tests := []struct {
		name      string
		modify    func(*CreateInput)
		base      Channel
		wantField string // empty means the input must be valid
	}{
		{name: "valid sms", base: ChannelSMS},
		{name: "valid email", base: ChannelEmail},
		{name: "valid push", base: ChannelPush},
		{name: "unknown channel", base: ChannelSMS, modify: func(in *CreateInput) { in.Channel = "fax" }, wantField: "channel"},
		{name: "unknown priority", base: ChannelSMS, modify: func(in *CreateInput) { in.Priority = "urgent" }, wantField: "priority"},
		{name: "missing recipient", base: ChannelSMS, modify: func(in *CreateInput) { in.Recipient = "" }, wantField: "recipient"},
		{name: "sms without plus", base: ChannelSMS, modify: func(in *CreateInput) { in.Recipient = "905551234567" }, wantField: "recipient"},
		{name: "sms with letters", base: ChannelSMS, modify: func(in *CreateInput) { in.Recipient = "+90555abc4567" }, wantField: "recipient"},
		{name: "bad email", base: ChannelEmail, modify: func(in *CreateInput) { in.Recipient = "not-an-email" }, wantField: "recipient"},
		{name: "email with display name", base: ChannelEmail, modify: func(in *CreateInput) { in.Recipient = "Bob <bob@example.com>" }, wantField: "recipient"},
		{name: "push token with space", base: ChannelPush, modify: func(in *CreateInput) { in.Recipient = "abc def" }, wantField: "recipient"},
		{name: "blank content", base: ChannelSMS, modify: func(in *CreateInput) { in.Content = "   " }, wantField: "content"},
		{name: "sms at limit", base: ChannelSMS, modify: func(in *CreateInput) { in.Content = strings.Repeat("a", MaxSMSLength) }},
		{name: "sms over limit", base: ChannelSMS, modify: func(in *CreateInput) { in.Content = strings.Repeat("a", MaxSMSLength+1) }, wantField: "content"},
		// Limits count characters, not bytes: "ğ" is 2 bytes in UTF-8.
		{name: "multibyte sms at limit", base: ChannelSMS, modify: func(in *CreateInput) { in.Content = strings.Repeat("ğ", MaxSMSLength) }},
		{name: "push over limit", base: ChannelPush, modify: func(in *CreateInput) { in.Content = strings.Repeat("a", MaxPushLength+1) }, wantField: "content"},
		{name: "scheduled in future", base: ChannelSMS, modify: func(in *CreateInput) { in.ScheduledAt = &future }},
		{name: "scheduled in past", base: ChannelSMS, modify: func(in *CreateInput) { in.ScheduledAt = &past }, wantField: "scheduled_at"},
		{name: "scheduled too far", base: ChannelSMS, modify: func(in *CreateInput) { in.ScheduledAt = &tooFar }, wantField: "scheduled_at"},
		{name: "idempotency key too long", base: ChannelSMS, modify: func(in *CreateInput) { in.IdempotencyKey = strings.Repeat("k", MaxIdempotencyKeyLength+1) }, wantField: "idempotency_key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := valid(tt.base)
			if tt.modify != nil {
				tt.modify(&in)
			}
			errs := in.Validate("", now)

			if tt.wantField == "" {
				if len(errs) != 0 {
					t.Fatalf("expected valid input, got errors: %+v", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Field != tt.wantField {
				t.Fatalf("expected one error on %q, got %+v", tt.wantField, errs)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	in := CreateInput{Recipient: "  +905551234567 ", Channel: " SMS ", Content: "hi"}
	in.Normalize()

	if in.Recipient != "+905551234567" {
		t.Errorf("recipient not trimmed: %q", in.Recipient)
	}
	if in.Channel != ChannelSMS {
		t.Errorf("channel not lower-cased: %q", in.Channel)
	}
	if in.Priority != PriorityNormal {
		t.Errorf("priority should default to normal, got %q", in.Priority)
	}
}

func TestValidateBatch(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if errs := ValidateBatch(nil, now); len(errs) != 1 {
			t.Fatalf("expected 1 error, got %+v", errs)
		}
	})

	t.Run("too many", func(t *testing.T) {
		items := make([]CreateInput, MaxBatchSize+1)
		for i := range items {
			items[i] = valid(ChannelSMS)
		}
		if errs := ValidateBatch(items, now); len(errs) != 1 {
			t.Fatalf("expected 1 error, got %d", len(errs))
		}
	})

	t.Run("errors are indexed", func(t *testing.T) {
		bad := valid(ChannelSMS)
		bad.Recipient = "nope"
		errs := ValidateBatch([]CreateInput{valid(ChannelSMS), bad}, now)
		if len(errs) != 1 || errs[0].Field != "notifications[1].recipient" {
			t.Fatalf("expected error on notifications[1].recipient, got %+v", errs)
		}
	})

	t.Run("duplicate idempotency keys", func(t *testing.T) {
		a, b := valid(ChannelSMS), valid(ChannelEmail)
		a.IdempotencyKey, b.IdempotencyKey = "same", "same"
		errs := ValidateBatch([]CreateInput{a, b}, now)
		if len(errs) != 1 || errs[0].Field != "notifications[1].idempotency_key" {
			t.Fatalf("expected duplicate-key error, got %+v", errs)
		}
	})
}

func TestPriorityRankRoundTrip(t *testing.T) {
	for _, p := range []Priority{PriorityHigh, PriorityNormal, PriorityLow} {
		if got := priorityFromRank(p.Rank()); got != p {
			t.Errorf("%s: round trip gave %s", p, got)
		}
	}
	if !(PriorityHigh.Rank() < PriorityNormal.Rank() && PriorityNormal.Rank() < PriorityLow.Rank()) {
		t.Error("high must sort before normal, and normal before low")
	}
}

func TestNewUsesScheduleAsNextAttempt(t *testing.T) {
	at := now.Add(2 * time.Hour)
	in := valid(ChannelSMS)
	in.ScheduledAt = &at

	n := New(in, nil, "corr-1", 5, now)
	if !n.NextAttemptAt.Equal(at) {
		t.Errorf("next_attempt_at = %v, want %v", n.NextAttemptAt, at)
	}
	if n.Status != StatusPending || n.IdempotencyKey != nil || *n.CorrelationID != "corr-1" {
		t.Errorf("unexpected notification: %+v", n)
	}
}
