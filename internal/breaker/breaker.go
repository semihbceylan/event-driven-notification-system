// Package breaker implements a circuit breaker.
//
// Without one, a provider outage is expensive: every message is tried, fails,
// and uses up one of its attempts. After a couple of minutes of outage, every
// queued message has used all its attempts and is marked failed, even though
// nothing was wrong with the messages themselves.
//
// The breaker watches the results for one channel:
//
//	closed    normal operation. After Threshold consecutive temporary
//	          failures it opens.
//	open      nothing is sent, so no attempts are used. Messages simply wait
//	          in the queue. After the cooldown it becomes half-open.
//	half-open exactly one "probe" message is sent. Success closes the
//	          breaker; failure opens it again with a doubled cooldown (up to
//	          MaxCooldown), so a long outage is probed less and less often.
//
// The breaker is per worker process. With several replicas, each one learns
// about an outage on its own after Threshold failures, which costs a few
// extra attempts but keeps the design simple.
package breaker

import (
	"sync"
	"time"
)

type State int

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

type Config struct {
	Threshold   int           // consecutive failures that open the breaker
	Cooldown    time.Duration // first open period
	MaxCooldown time.Duration // cap for the doubling cooldown
}

type Breaker struct {
	cfg Config
	now func() time.Time

	mu            sync.Mutex
	state         State
	failures      int
	cooldown      time.Duration
	openUntil     time.Time
	probeInFlight bool
	onChange      func(from, to State)
}

func New(cfg Config) *Breaker {
	return &Breaker{cfg: cfg, now: time.Now, cooldown: cfg.Cooldown}
}

// OnStateChange registers a callback for logging and metrics. It runs with
// the breaker's lock held, so it must be quick and must not call the breaker.
func (b *Breaker) OnStateChange(fn func(from, to State)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onChange = fn
}

// Allow reports whether a send may go ahead. In half-open state it returns
// true for exactly one caller (the probe) until that probe reports back.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Before(b.openUntil) {
			return false
		}
		b.setState(HalfOpen)
		fallthrough
	default: // HalfOpen
		if b.probeInFlight {
			return false
		}
		b.probeInFlight = true
		return true
	}
}

// Success records a successful send and closes the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.probeInFlight = false
	b.cooldown = b.cfg.Cooldown
	if b.state != Closed {
		b.setState(Closed)
	}
}

// Failure records a temporary failure (5xx, timeout, 429). retryAfter is the
// provider's Retry-After, if it sent one: the provider is telling us
// directly to back off, so the breaker opens at once for at least that long.
func (b *Breaker) Failure(retryAfter time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++

	switch {
	case b.state == HalfOpen:
		// The probe failed: the provider is still down. Wait longer next time.
		b.cooldown = min(b.cooldown*2, b.cfg.MaxCooldown)
		b.open(max(b.cooldown, retryAfter))
	case retryAfter > 0:
		b.open(max(b.cooldown, retryAfter))
	case b.state == Closed && b.failures >= b.cfg.Threshold:
		b.open(b.cooldown)
	}
}

// Release is called when an allowed send did not happen (for example, the
// queue turned out to be empty), so a half-open breaker can let another
// probe through.
func (b *Breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probeInFlight = false
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Breaker) open(d time.Duration) {
	b.probeInFlight = false
	b.openUntil = b.now().Add(d)
	if b.state != Open {
		b.setState(Open)
	}
}

func (b *Breaker) setState(to State) {
	from := b.state
	b.state = to
	if b.onChange != nil {
		b.onChange(from, to)
	}
}
