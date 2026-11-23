package notification

import (
	"context"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	lock := time.Minute
	past := time.Now().Add(-time.Minute)

	mk := func(ch Channel, nextAt time.Time) Notification {
		t.Helper()
		rcpt := map[Channel]string{ChannelSMS: "+905551234567", ChannelEmail: "a@example.com", ChannelPush: "tok"}[ch]
		n := New(CreateInput{Recipient: rcpt, Channel: ch, Content: "x", Priority: PriorityNormal}, nil, "", 2, time.Now())
		n.NextAttemptAt = nextAt
		if _, err := store.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// SMS: 3 sent (one needed a retry), 1 failed, 2 due, 1 scheduled for later.
	for range 3 {
		mk(ChannelSMS, past)
	}
	mk(ChannelSMS, past) // will fail
	for range 3 {
		n, _, _ := store.Claim(ctx, ChannelSMS, lock)
		if err := store.MarkSent(ctx, n, "m", Attempt{StartedAt: time.Now(), Duration: 40 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
	}
	n, _, _ := store.Claim(ctx, ChannelSMS, lock)
	if err := store.MarkFailed(ctx, n, Attempt{StartedAt: time.Now(), StatusCode: 400, Error: "bad", Duration: 10 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	mk(ChannelSMS, past)
	mk(ChannelSMS, past)
	mk(ChannelSMS, time.Now().Add(time.Hour))
	// One email claimed and still in flight.
	mk(ChannelEmail, past)
	if _, ok, _ := store.Claim(ctx, ChannelEmail, lock); !ok {
		t.Fatal("claim email")
	}
	// An old send outside the window must not count.
	old := mk(ChannelPush, past)
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status='sent', sent_at=now() - interval '2 hours' WHERE id=$1`, old.ID); err != nil {
		t.Fatal(err)
	}

	stats, err := store.Stats(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	sms := stats[ChannelSMS]
	if sms.Queue != (QueueDepth{Due: 2, Waiting: 1, Processing: 0}) {
		t.Errorf("sms queue = %+v", sms.Queue)
	}
	if sms.Sent != 3 || sms.Failed != 1 || sms.Attempts != 4 {
		t.Errorf("sms sent=%d failed=%d attempts=%d, want 3 1 4", sms.Sent, sms.Failed, sms.Attempts)
	}
	if sms.SuccessRate == nil || *sms.SuccessRate != 0.75 {
		t.Errorf("sms success rate = %v, want 0.75", sms.SuccessRate)
	}
	if sms.ProviderLatencyMs.P50 == nil || *sms.ProviderLatencyMs.P50 != 40 {
		t.Errorf("sms provider p50 = %v, want 40", sms.ProviderLatencyMs.P50)
	}
	if sms.DeliveryLatencyMs.P50 == nil {
		t.Error("sms delivery latency missing")
	}

	if stats[ChannelEmail].Queue.Processing != 1 {
		t.Errorf("email processing = %d, want 1", stats[ChannelEmail].Queue.Processing)
	}
	push := stats[ChannelPush]
	if push.Sent != 0 || push.SuccessRate != nil || push.ProviderLatencyMs.P50 != nil {
		t.Errorf("push should be empty within the window: %+v", push)
	}
}
