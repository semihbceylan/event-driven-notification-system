package breaker

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestBreaker() (*Breaker, *clock) {
	c := &clock{t: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)}
	b := New(Config{Threshold: 3, Cooldown: 10 * time.Second, MaxCooldown: 40 * time.Second})
	b.now = c.now
	return b, c
}

func TestOpensAfterThreshold(t *testing.T) {
	b, _ := newTestBreaker()
	for range 2 {
		b.Failure(0)
	}
	if b.State() != Closed || !b.Allow() {
		t.Fatal("must stay closed below the threshold")
	}
	b.Failure(0)
	if b.State() != Open || b.Allow() {
		t.Fatal("must open at the threshold and block sends")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b, _ := newTestBreaker()
	b.Failure(0)
	b.Failure(0)
	b.Success()
	b.Failure(0)
	b.Failure(0)
	if b.State() != Closed {
		t.Fatal("failures must be consecutive to open the breaker")
	}
}

func TestHalfOpenAllowsOneProbe(t *testing.T) {
	b, c := newTestBreaker()
	for range 3 {
		b.Failure(0)
	}
	c.advance(10 * time.Second)

	if !b.Allow() {
		t.Fatal("after the cooldown, one probe must be allowed")
	}
	if b.State() != HalfOpen {
		t.Fatalf("state = %s, want half-open", b.State())
	}
	if b.Allow() {
		t.Fatal("only one probe at a time")
	}

	b.Success()
	if b.State() != Closed || !b.Allow() {
		t.Fatal("a successful probe must close the breaker")
	}
}

func TestFailedProbeDoublesCooldownUpToMax(t *testing.T) {
	b, c := newTestBreaker()
	for range 3 {
		b.Failure(0)
	}
	for _, want := range []time.Duration{20 * time.Second, 40 * time.Second, 40 * time.Second} {
		c.advance(time.Hour) // well past any cooldown
		if !b.Allow() {
			t.Fatal("probe should be allowed")
		}
		b.Failure(0) // probe fails

		c.advance(want - time.Millisecond)
		if b.Allow() {
			t.Fatalf("still open just before %v", want)
		}
		c.advance(time.Millisecond)
		if !b.Allow() {
			t.Fatalf("should allow a probe after %v", want)
		}
		b.Release()
	}
}

func TestRetryAfterOpensImmediately(t *testing.T) {
	b, c := newTestBreaker()
	b.Failure(30 * time.Second) // a single 429 with Retry-After: 30
	if b.State() != Open {
		t.Fatal("Retry-After must open the breaker at once")
	}
	c.advance(29 * time.Second)
	if b.Allow() {
		t.Fatal("must respect the full Retry-After")
	}
	c.advance(time.Second)
	if !b.Allow() {
		t.Fatal("should probe once Retry-After has passed")
	}
}

func TestReleaseLetsAnotherProbeThrough(t *testing.T) {
	b, c := newTestBreaker()
	for range 3 {
		b.Failure(0)
	}
	c.advance(10 * time.Second)
	if !b.Allow() {
		t.Fatal("probe should be allowed")
	}
	b.Release() // e.g. the queue was empty
	if !b.Allow() {
		t.Fatal("after Release another caller may probe")
	}
}

func TestConcurrentAllowGivesOneProbe(t *testing.T) {
	b, c := newTestBreaker()
	for range 3 {
		b.Failure(0)
	}
	c.advance(10 * time.Second)

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Allow() {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 1 {
		t.Fatalf("%d goroutines got a probe, want 1", allowed.Load())
	}
}

func TestStateChangeCallback(t *testing.T) {
	b, c := newTestBreaker()
	var changes []string
	b.OnStateChange(func(from, to State) { changes = append(changes, from.String()+"->"+to.String()) })

	for range 3 {
		b.Failure(0)
	}
	c.advance(10 * time.Second)
	b.Allow()
	b.Success()

	want := []string{"closed->open", "open->half-open", "half-open->closed"}
	if len(changes) != len(want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
	for i := range want {
		if changes[i] != want[i] {
			t.Errorf("change %d = %s, want %s", i, changes[i], want[i])
		}
	}
}
