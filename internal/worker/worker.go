// Package worker delivers queued notifications.
//
// Each channel gets its own pool of sender goroutines. A sender loops:
//
//	claim the next due notification -> wait for a rate-limit slot -> send it
//	-> record the result (sent, retry later, or failed)
//
// Channels have separate pools so a slow or failing provider for one channel
// (say email) cannot hold up the others.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
	"github.com/semihbceylan/event-driven-notification-system/internal/provider"
)

// Queue is the part of notification.Store the worker uses.
type Queue interface {
	Claim(ctx context.Context, ch notification.Channel, lockFor time.Duration) (notification.Notification, bool, error)
	MarkSent(ctx context.Context, n notification.Notification, providerMessageID string, a notification.Attempt) error
	MarkRetry(ctx context.Context, n notification.Notification, nextAttemptAt time.Time, a notification.Attempt) error
	MarkFailed(ctx context.Context, n notification.Notification, a notification.Attempt) error
	Release(ctx context.Context, n notification.Notification) error
	RequeueStuck(ctx context.Context) (int64, error)
}

type Sender interface {
	Send(ctx context.Context, req provider.Request) (provider.Response, error)
}

type Limiter interface {
	Wait(ctx context.Context, key string) error
}

type Config struct {
	Channels          []notification.Channel
	WorkersPerChannel int
	// PollInterval is how long an idle sender sleeps when the queue is empty.
	PollInterval time.Duration
	// LockTimeout is how long a claimed notification stays reserved. It must
	// be longer than a rate-limit wait plus a provider call.
	LockTimeout    time.Duration
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	// ReapInterval is how often stuck notifications are put back in the queue.
	ReapInterval time.Duration
}

type Worker struct {
	cfg     Config
	queue   Queue
	sender  Sender
	limiter Limiter
	logger  *slog.Logger

	now  func() time.Time
	rand func() float64
}

func New(cfg Config, q Queue, s Sender, l Limiter, logger *slog.Logger) *Worker {
	return &Worker{cfg: cfg, queue: q, sender: s, limiter: l, logger: logger, now: time.Now, rand: rand.Float64}
}

// Run starts all senders and the reaper, and blocks until ctx is cancelled
// and in-flight sends have finished.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, ch := range w.cfg.Channels {
		for range w.cfg.WorkersPerChannel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w.senderLoop(ctx, ch)
			}()
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reaperLoop(ctx)
	}()
	wg.Wait()
}

func (w *Worker) senderLoop(ctx context.Context, ch notification.Channel) {
	for ctx.Err() == nil {
		worked, err := w.ProcessOne(ctx, ch)
		switch {
		case err != nil:
			w.logger.Error("processing failed", "channel", ch, "error", err)
			sleep(ctx, time.Second) // e.g. database down: do not spin
		case !worked:
			sleep(ctx, w.cfg.PollInterval)
		}
	}
}

// ProcessOne claims and delivers at most one notification. worked is false
// if the queue had nothing due.
func (w *Worker) ProcessOne(ctx context.Context, ch notification.Channel) (worked bool, err error) {
	n, ok, err := w.queue.Claim(ctx, ch, w.cfg.LockTimeout)
	if err != nil || !ok {
		return false, err
	}

	log := w.logger.With(
		"notification_id", n.ID,
		"channel", n.Channel,
		"attempt", n.Attempts,
		"correlation_id", deref(n.CorrelationID),
	)

	// From here on, finish the job even if shutdown starts: a half-done send
	// is worse than a slightly slower shutdown.
	jobCtx := context.WithoutCancel(ctx)

	if err := w.limiter.Wait(ctx, string(ch)); err != nil {
		// Shutting down, or Redis is unreachable. We never send without a
		// rate-limit slot, so hand the notification back untouched.
		if relErr := w.queue.Release(jobCtx, n); relErr != nil {
			log.Warn("could not release notification", "error", relErr)
		}
		if errors.Is(err, context.Canceled) {
			return true, nil
		}
		return true, err
	}

	start := w.now()
	resp, sendErr := w.sender.Send(jobCtx, provider.Request{
		To:             n.Recipient,
		Channel:        string(n.Channel),
		Content:        n.Content,
		IdempotencyKey: n.ID.String(),
		CorrelationID:  deref(n.CorrelationID),
	})
	attempt := notification.Attempt{StartedAt: start, Duration: w.now().Sub(start)}

	if sendErr == nil {
		if err = w.queue.MarkSent(jobCtx, n, resp.MessageID, attempt); err == nil {
			log.Info("notification sent", "provider_message_id", resp.MessageID, "duration_ms", attempt.Duration.Milliseconds())
		}
		return true, w.checkRecord(log, err)
	}

	pe := provider.AsError(sendErr)
	attempt.StatusCode = pe.StatusCode
	attempt.Error = pe.Error()

	if !pe.Retryable || n.Attempts >= n.MaxAttempts {
		err = w.queue.MarkFailed(jobCtx, n, attempt)
		log.Warn("notification failed permanently", "error", attempt.Error, "status_code", pe.StatusCode,
			"retryable", pe.Retryable, "max_attempts", n.MaxAttempts)
		return true, w.checkRecord(log, err)
	}

	delay := Backoff(n.Attempts, w.cfg.RetryBaseDelay, w.cfg.RetryMaxDelay, w.rand)
	if pe.RetryAfter > delay {
		delay = pe.RetryAfter // the provider told us how long to back off
	}
	err = w.queue.MarkRetry(jobCtx, n, w.now().Add(delay), attempt)
	log.Info("notification will be retried", "error", attempt.Error, "status_code", pe.StatusCode, "retry_in", delay.String())
	return true, w.checkRecord(log, err)
}

// checkRecord treats a lost lock as expected (logged, not an error): the
// notification was already handed to another worker.
func (w *Worker) checkRecord(log *slog.Logger, err error) error {
	if errors.Is(err, notification.ErrLockLost) {
		log.Warn("result not recorded: lock expired and the notification was requeued")
		return nil
	}
	return err
}

func (w *Worker) reaperLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := w.queue.RequeueStuck(ctx)
			if err != nil {
				w.logger.Error("requeue stuck notifications", "error", err)
			} else if n > 0 {
				w.logger.Warn("requeued stuck notifications", "count", n)
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
