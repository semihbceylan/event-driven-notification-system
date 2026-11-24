package ratelimit

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/testcontainers/testcontainers-go"
)

func newRedis(t *testing.T) *redis.Client {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test; needs Docker")
	}
	// Locally, skip when Docker is not running instead of failing. CI sets
	// CI=true, so there a missing Docker still fails the build.
	if os.Getenv("CI") == "" {
		testcontainers.SkipIfProviderIsNotHealthy(t)
	}
	ctx := context.Background()
	ctr, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })

	url, err := ctr.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestLimiter(t *testing.T) {
	rdb := newRedis(t)

	t.Run("never exceeds the limit in any window, across concurrent callers", func(t *testing.T) {
		// Simulates several worker processes sharing one limit:
		// 10 per 200ms, 8 goroutines, 40 acquisitions in total.
		const limit, total = 10, 40
		window := 200 * time.Millisecond
		l := New(rdb, limit, window)

		var (
			mu    sync.Mutex
			times []time.Time
			left  atomic.Int32
			wg    sync.WaitGroup
		)
		left.Store(total)
		start := time.Now()
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for left.Add(-1) >= 0 {
					if err := l.Wait(context.Background(), "concurrent"); err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					times = append(times, time.Now())
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		// 40 events at 10 per window need at least 3 full windows.
		if elapsed := time.Since(start); elapsed < 3*window {
			t.Errorf("40 events finished in %v; the limit allows no faster than %v", elapsed, 3*window)
		}
		// Sliding check: no window-long span may contain more than limit
		// events. The small tolerance covers only the delay between Redis
		// granting a slot and this goroutine reading the clock; a larger one
		// once hid a millisecond-rounding bug in the limiter.
		tolerance := 2 * time.Millisecond
		for i := range times {
			count := 0
			for j := range times {
				if !times[j].Before(times[i]) && times[j].Sub(times[i]) < window-tolerance {
					count++
				}
			}
			if count > limit {
				t.Fatalf("%d events within one window starting at event %d; limit is %d", count, i, limit)
			}
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		l := New(rdb, 5, time.Minute)
		ctx := context.Background()
		for range 5 {
			if err := l.Wait(ctx, "sms"); err != nil {
				t.Fatal(err)
			}
		}
		// sms is now exhausted for a minute; email must still go through at once.
		ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if err := l.Wait(ctx, "email"); err != nil {
			t.Errorf("email blocked by sms limit: %v", err)
		}
	})

	t.Run("wait respects context cancellation", func(t *testing.T) {
		l := New(rdb, 1, time.Minute)
		if err := l.Wait(context.Background(), "cancel"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := l.Wait(ctx, "cancel"); err == nil {
			t.Error("expected context error while waiting for a slot")
		}
	})
}
