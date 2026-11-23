// Package worker delivers queued notifications.
//
// Each channel gets its own pool of sender goroutines. A sender loops:
//
//	check the channel's circuit breaker -> claim the next due notification
//	-> wait for a rate-limit slot -> send it -> record the result
//	(sent, retry later, or failed)
//
// Channels have separate pools and breakers so a slow or failing provider
// for one channel (say email) cannot hold up the others.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/semihbceylan/event-driven-notification-system/internal/breaker"
	"github.com/semihbceylan/event-driven-notification-system/internal/events"
	"github.com/semihbceylan/event-driven-notification-system/internal/metrics"
	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
	"github.com/semihbceylan/event-driven-notification-system/internal/provider"
	"github.com/semihbceylan/event-driven-notification-system/internal/tracing"
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

// EventPublisher broadcasts status changes to live clients (the dashboard).
// Optional; publishing is best-effort and never fails a delivery.
type EventPublisher interface {
	Publish(ctx context.Context, evs ...events.Event)
}

type Config struct {
	Channels          []notification.Channel
	WorkersPerChannel int
	// PollInterval is how long an idle sender sleeps when the queue is empty
	// or the channel's breaker is open.
	PollInterval time.Duration
	// LockTimeout is how long a claimed notification stays reserved. It must
	// be longer than a rate-limit wait plus a provider call.
	LockTimeout    time.Duration
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	// ReapInterval is how often stuck notifications are put back in the queue.
	ReapInterval time.Duration
	Breaker      breaker.Config
}

type Worker struct {
	cfg      Config
	queue    Queue
	sender   Sender
	limiter  Limiter
	logger   *slog.Logger
	metrics  *metrics.Worker
	breakers map[notification.Channel]*breaker.Breaker
	events   EventPublisher

	now  func() time.Time
	rand func() float64
}

// New creates a worker. m may be nil, in which case metrics go to a private
// registry that is never exported (handy in tests).
func New(cfg Config, q Queue, s Sender, l Limiter, m *metrics.Worker, logger *slog.Logger) *Worker {
	if m == nil {
		m = metrics.NewWorker(prometheus.NewRegistry())
	}
	w := &Worker{
		cfg: cfg, queue: q, sender: s, limiter: l, metrics: m, logger: logger,
		breakers: make(map[notification.Channel]*breaker.Breaker),
		now:      time.Now, rand: rand.Float64,
	}
	for _, ch := range cfg.Channels {
		b := breaker.New(cfg.Breaker)
		b.OnStateChange(func(from, to breaker.State) {
			m.BreakerState(string(ch), int(to))
			if to == breaker.Open {
				logger.Warn("circuit breaker opened: pausing channel", "channel", ch, "from", from.String())
			} else {
				logger.Info("circuit breaker state changed", "channel", ch, "from", from.String(), "to", to.String())
			}
		})
		m.BreakerState(string(ch), int(breaker.Closed))
		w.breakers[ch] = b
	}
	return w
}

// SetEventPublisher enables live status events.
func (w *Worker) SetEventPublisher(p EventPublisher) { w.events = p }

// emit publishes n's new state. A failed record (e.g. lost lock) is not
// announced, since the change did not happen.
func (w *Worker) emit(ctx context.Context, n notification.Notification, status notification.Status, lastErr string, recordErr error) {
	if w.events == nil || recordErr != nil {
		return
	}
	n.Status = status
	if lastErr != "" {
		n.LastError = &lastErr
	}
	w.events.Publish(ctx, events.FromNotification(n, w.now()))
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
// if nothing was done: the queue had nothing due, or the channel's breaker
// is open.
func (w *Worker) ProcessOne(ctx context.Context, ch notification.Channel) (worked bool, err error) {
	br := w.breakers[ch]
	// While the breaker is open we do not even claim: queued notifications
	// keep all their attempts until the provider is back.
	if br != nil && !br.Allow() {
		return false, nil
	}
	sent := false
	defer func() {
		if br != nil && !sent {
			br.Release() // let another sender probe if this one never sent
		}
	}()

	n, ok, err := w.queue.Claim(ctx, ch, w.cfg.LockTimeout)
	if err != nil || !ok {
		return false, err
	}

	// Continue the trace of the API request that created the notification,
	// so this attempt appears in the same trace (see internal/tracing).
	ctx, span := otel.Tracer("worker").Start(
		tracing.WithTraceParent(ctx, deref(n.TraceParent)),
		"deliver "+string(n.Channel),
		trace.WithAttributes(
			attribute.String("notification.id", n.ID.String()),
			attribute.String("notification.channel", string(n.Channel)),
			attribute.String("notification.priority", string(n.Priority)),
			attribute.Int("notification.attempt", n.Attempts),
			attribute.String("correlation_id", deref(n.CorrelationID)),
		),
	)
	defer span.End()

	w.emit(ctx, n, notification.StatusProcessing, "", nil)

	log := w.logger.With(
		"notification_id", n.ID,
		"channel", n.Channel,
		"attempt", n.Attempts,
		"correlation_id", deref(n.CorrelationID),
	)
	if traceID := tracing.TraceID(ctx); traceID != "" {
		log = log.With("trace_id", traceID)
	}

	// From here on, finish the job even if shutdown starts: a half-done send
	// is worse than a slightly slower shutdown.
	jobCtx := context.WithoutCancel(ctx)

	waitStart := w.now()
	if err := w.limiter.Wait(ctx, string(ch)); err != nil {
		// Shutting down, or Redis is unreachable. We never send without a
		// rate-limit slot, so hand the notification back untouched.
		relErr := w.queue.Release(jobCtx, n)
		if relErr != nil {
			log.Warn("could not release notification", "error", relErr)
		}
		n.Attempts--
		w.emit(jobCtx, n, notification.StatusPending, "", relErr)
		if errors.Is(err, context.Canceled) {
			return true, nil
		}
		return true, err
	}
	w.metrics.RateLimitWait(string(ch), w.now().Sub(waitStart))

	start := w.now()
	resp, sendErr := w.sender.Send(jobCtx, provider.Request{
		To:             n.Recipient,
		Channel:        string(n.Channel),
		Content:        n.Content,
		IdempotencyKey: n.ID.String(),
		CorrelationID:  deref(n.CorrelationID),
	})
	sent = true
	attempt := notification.Attempt{StartedAt: start, Duration: w.now().Sub(start)}

	if sendErr == nil {
		span.SetAttributes(attribute.String("notification.outcome", "sent"))
		if br != nil {
			br.Success()
		}
		w.metrics.ProviderCall(string(ch), "success", attempt.Duration)
		w.metrics.Processed(string(ch), "sent")
		w.metrics.Delivered(string(ch), string(n.Priority), w.now().Sub(dueAt(n)))
		if err = w.queue.MarkSent(jobCtx, n, resp.MessageID, attempt); err == nil {
			log.Info("notification sent", "provider_message_id", resp.MessageID, "duration_ms", attempt.Duration.Milliseconds())
		}
		w.emit(jobCtx, n, notification.StatusSent, "", err)
		return true, w.checkRecord(log, err)
	}

	pe := provider.AsError(sendErr)
	attempt.StatusCode = pe.StatusCode
	attempt.Error = pe.Error()
	span.SetStatus(codes.Error, attempt.Error)
	span.SetAttributes(attribute.Bool("provider.retryable", pe.Retryable))

	if br != nil {
		if pe.Retryable {
			br.Failure(pe.RetryAfter)
		} else {
			// A 4xx means the provider is up and rejected this one message.
			br.Success()
		}
	}
	result := "permanent_error"
	if pe.Retryable {
		result = "retryable_error"
	}
	w.metrics.ProviderCall(string(ch), result, attempt.Duration)

	if !pe.Retryable || n.Attempts >= n.MaxAttempts {
		span.SetAttributes(attribute.String("notification.outcome", "failed"))
		w.metrics.Processed(string(ch), "failed")
		err = w.queue.MarkFailed(jobCtx, n, attempt)
		w.emit(jobCtx, n, notification.StatusFailed, attempt.Error, err)
		log.Warn("notification failed permanently", "error", attempt.Error, "status_code", pe.StatusCode,
			"retryable", pe.Retryable, "max_attempts", n.MaxAttempts)
		return true, w.checkRecord(log, err)
	}

	delay := Backoff(n.Attempts, w.cfg.RetryBaseDelay, w.cfg.RetryMaxDelay, w.rand)
	if pe.RetryAfter > delay {
		delay = pe.RetryAfter // the provider told us how long to back off
	}
	span.SetAttributes(attribute.String("notification.outcome", "retry"), attribute.String("retry.in", delay.String()))
	w.metrics.Processed(string(ch), "retry")
	err = w.queue.MarkRetry(jobCtx, n, w.now().Add(delay), attempt)
	w.emit(jobCtx, n, notification.StatusPending, attempt.Error, err)
	log.Info("notification will be retried", "error", attempt.Error, "status_code", pe.StatusCode, "retry_in", delay.String())
	return true, w.checkRecord(log, err)
}

// BreakerStates reports each channel's breaker state, for the health check.
func (w *Worker) BreakerStates() map[string]string {
	out := make(map[string]string, len(w.breakers))
	for ch, b := range w.breakers {
		out[string(ch)] = b.State().String()
	}
	return out
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

// dueAt is when the notification became ready to send.
func dueAt(n notification.Notification) time.Time {
	if n.ScheduledAt != nil {
		return *n.ScheduledAt
	}
	return n.CreatedAt
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
