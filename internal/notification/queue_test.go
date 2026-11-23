package notification

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestQueue(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()
	lock := time.Minute

	create := func(t *testing.T, ch Channel, p Priority, nextAt time.Time) Notification {
		t.Helper()
		rcpt := map[Channel]string{ChannelSMS: "+905551234567", ChannelEmail: "a@example.com", ChannelPush: "tok"}[ch]
		n := New(CreateInput{Recipient: rcpt, Channel: ch, Content: "x", Priority: p}, nil, "", 3, time.Now())
		n.NextAttemptAt = nextAt
		if _, err := store.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	reset := func(t *testing.T) {
		t.Helper()
		if _, err := pool.Exec(ctx, `TRUNCATE notifications CASCADE`); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Minute)

	t.Run("claims by priority, then oldest first, and only its channel", func(t *testing.T) {
		reset(t)
		low := create(t, ChannelPush, PriorityLow, past.Add(-time.Hour))
		normalOld := create(t, ChannelPush, PriorityNormal, past.Add(-time.Minute))
		normalNew := create(t, ChannelPush, PriorityNormal, past)
		high := create(t, ChannelPush, PriorityHigh, past)
		create(t, ChannelSMS, PriorityHigh, past) // other channel

		var order []uuid.UUID
		for {
			n, ok, err := store.Claim(ctx, ChannelPush, lock)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			if n.Status != StatusProcessing || n.Attempts != 1 {
				t.Errorf("claimed row: status=%s attempts=%d", n.Status, n.Attempts)
			}
			order = append(order, n.ID)
		}
		want := []uuid.UUID{high.ID, normalOld.ID, normalNew.ID, low.ID}
		if len(order) != len(want) {
			t.Fatalf("claimed %d rows, want %d", len(order), len(want))
		}
		for i := range want {
			if order[i] != want[i] {
				t.Errorf("position %d: got %v, want %v", i, order[i], want[i])
			}
		}
	})

	t.Run("does not claim scheduled or cancelled notifications", func(t *testing.T) {
		reset(t)
		create(t, ChannelSMS, PriorityHigh, time.Now().Add(time.Hour)) // scheduled for later
		c := create(t, ChannelSMS, PriorityHigh, past)
		if _, err := store.Cancel(ctx, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := store.Claim(ctx, ChannelSMS, lock); ok {
			t.Error("claimed a notification that is not due")
		}
	})

	t.Run("concurrent workers never claim the same row", func(t *testing.T) {
		reset(t)
		const rows = 50
		for range rows {
			create(t, ChannelEmail, PriorityNormal, past)
		}

		var (
			mu      sync.Mutex
			claimed = map[uuid.UUID]int{}
			wg      sync.WaitGroup
		)
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					n, ok, err := store.Claim(ctx, ChannelEmail, lock)
					if err != nil {
						t.Error(err)
						return
					}
					if !ok {
						return
					}
					mu.Lock()
					claimed[n.ID]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		if len(claimed) != rows {
			t.Errorf("claimed %d distinct rows, want %d", len(claimed), rows)
		}
		for id, times := range claimed {
			if times > 1 {
				t.Errorf("row %v claimed %d times", id, times)
			}
		}
	})

	t.Run("mark sent records the attempt", func(t *testing.T) {
		reset(t)
		create(t, ChannelSMS, PriorityNormal, past)
		n, _, _ := store.Claim(ctx, ChannelSMS, lock)
		if err := store.MarkSent(ctx, n, "prov-1", Attempt{StatusCode: 202, Duration: 30 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, n.ID)
		if got.Status != StatusSent || got.SentAt == nil || got.ProviderMessageID == nil || *got.ProviderMessageID != "prov-1" {
			t.Errorf("after MarkSent: %+v", got)
		}
		var attempts, code int
		_ = pool.QueryRow(ctx, `SELECT count(*), max(status_code) FROM delivery_attempts WHERE notification_id = $1`, n.ID).Scan(&attempts, &code)
		if attempts != 1 || code != 202 {
			t.Errorf("delivery_attempts: count=%d status=%d", attempts, code)
		}
	})

	t.Run("retry puts it back and it is claimable when due", func(t *testing.T) {
		reset(t)
		create(t, ChannelSMS, PriorityNormal, past)
		n, _, _ := store.Claim(ctx, ChannelSMS, lock)
		if err := store.MarkRetry(ctx, n, time.Now().Add(time.Hour), Attempt{StatusCode: 503, Error: "down"}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := store.Claim(ctx, ChannelSMS, lock); ok {
			t.Error("claimed before its retry time")
		}
		if _, err := pool.Exec(ctx, `UPDATE notifications SET next_attempt_at = now() - interval '1 second' WHERE id = $1`, n.ID); err != nil {
			t.Fatal(err)
		}
		again, ok, _ := store.Claim(ctx, ChannelSMS, lock)
		if !ok || again.Attempts != 2 || again.LastError == nil || *again.LastError != "down" {
			t.Errorf("second claim: ok=%v %+v", ok, again)
		}
	})

	t.Run("a stale worker cannot overwrite a newer attempt", func(t *testing.T) {
		reset(t)
		create(t, ChannelSMS, PriorityNormal, past)
		stale, _, _ := store.Claim(ctx, ChannelSMS, -time.Second) // lock already expired

		if n, err := store.RequeueStuck(ctx); err != nil || n != 1 {
			t.Fatalf("requeue: n=%d err=%v", n, err)
		}
		fresh, ok, _ := store.Claim(ctx, ChannelSMS, lock)
		if !ok || fresh.Attempts != 2 {
			t.Fatalf("reclaim: ok=%v attempts=%d", ok, fresh.Attempts)
		}

		// The first worker wakes up and tries to record its result.
		if err := store.MarkFailed(ctx, stale, Attempt{Error: "late"}); !errors.Is(err, ErrLockLost) {
			t.Errorf("stale result: err=%v, want ErrLockLost", err)
		}
		if err := store.MarkSent(ctx, fresh, "ok", Attempt{}); err != nil {
			t.Errorf("current owner could not record: %v", err)
		}
	})

	t.Run("stuck rows out of attempts become failed", func(t *testing.T) {
		reset(t)
		n := create(t, ChannelSMS, PriorityNormal, past)
		if _, err := pool.Exec(ctx, `UPDATE notifications SET status='processing', attempts=3, locked_until=now() - interval '1 second' WHERE id=$1`, n.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RequeueStuck(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, n.ID)
		if got.Status != StatusFailed {
			t.Errorf("status = %s, want failed (max_attempts is 3)", got.Status)
		}
	})

	t.Run("release gives the attempt back", func(t *testing.T) {
		reset(t)
		create(t, ChannelSMS, PriorityNormal, past)
		n, _, _ := store.Claim(ctx, ChannelSMS, lock)
		if err := store.Release(ctx, n); err != nil {
			t.Fatal(err)
		}
		again, ok, _ := store.Claim(ctx, ChannelSMS, lock)
		if !ok || again.Attempts != 1 {
			t.Errorf("after release: ok=%v attempts=%d, want 1", ok, again.Attempts)
		}
	})
}
