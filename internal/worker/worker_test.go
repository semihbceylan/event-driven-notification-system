package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
	"github.com/semihbceylan/event-driven-notification-system/internal/provider"
)

// fakeQueue hands out one notification and records what the worker did
// with it.
type fakeQueue struct {
	next     *notification.Notification
	sent     bool
	failed   bool
	released bool
	retryAt  time.Time
	attempt  notification.Attempt
	recordFn func() error
}

func (q *fakeQueue) Claim(context.Context, notification.Channel, time.Duration) (notification.Notification, bool, error) {
	if q.next == nil {
		return notification.Notification{}, false, nil
	}
	n := *q.next
	q.next = nil
	return n, true, nil
}

func (q *fakeQueue) record() error {
	if q.recordFn != nil {
		return q.recordFn()
	}
	return nil
}

func (q *fakeQueue) MarkSent(_ context.Context, _ notification.Notification, _ string, a notification.Attempt) error {
	q.sent, q.attempt = true, a
	return q.record()
}

func (q *fakeQueue) MarkRetry(_ context.Context, _ notification.Notification, at time.Time, a notification.Attempt) error {
	q.retryAt, q.attempt = at, a
	return q.record()
}

func (q *fakeQueue) MarkFailed(_ context.Context, _ notification.Notification, a notification.Attempt) error {
	q.failed, q.attempt = true, a
	return q.record()
}

func (q *fakeQueue) Release(context.Context, notification.Notification) error {
	q.released = true
	return nil
}

func (q *fakeQueue) RequeueStuck(context.Context) (int64, error) { return 0, nil }

type fakeSender struct {
	err   error
	calls int
	last  provider.Request
}

func (s *fakeSender) Send(_ context.Context, req provider.Request) (provider.Response, error) {
	s.calls++
	s.last = req
	if s.err != nil {
		return provider.Response{}, s.err
	}
	return provider.Response{MessageID: "msg-1", Status: "accepted"}, nil
}

type fakeLimiter struct{ err error }

func (l fakeLimiter) Wait(context.Context, string) error { return l.err }

var fixedNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func newTestWorker(q Queue, s Sender, l Limiter) *Worker {
	w := New(Config{RetryBaseDelay: 2 * time.Second, RetryMaxDelay: time.Minute}, q, s, l,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.now = func() time.Time { return fixedNow }
	w.rand = func() float64 { return 0 } // lower edge of the jitter range
	return w
}

func pending(attempts int) *notification.Notification {
	corr := "corr-1"
	return &notification.Notification{
		ID: uuid.New(), Channel: notification.ChannelSMS, Recipient: "+905551234567", Content: "hi",
		Status: notification.StatusProcessing, Attempts: attempts, MaxAttempts: 5, CorrelationID: &corr,
	}
}

func TestProcessOneEmptyQueue(t *testing.T) {
	w := newTestWorker(&fakeQueue{}, &fakeSender{}, fakeLimiter{})
	worked, err := w.ProcessOne(context.Background(), notification.ChannelSMS)
	if worked || err != nil {
		t.Errorf("worked=%v err=%v, want false, nil", worked, err)
	}
}

func TestProcessOneSuccess(t *testing.T) {
	n := pending(1)
	q, s := &fakeQueue{next: n}, &fakeSender{}
	if _, err := newTestWorker(q, s, fakeLimiter{}).ProcessOne(context.Background(), n.Channel); err != nil {
		t.Fatal(err)
	}
	if !q.sent {
		t.Fatal("expected MarkSent")
	}
	if s.last.IdempotencyKey != n.ID.String() || s.last.CorrelationID != "corr-1" || s.last.To != n.Recipient {
		t.Errorf("provider request = %+v", s.last)
	}
}

func TestProcessOneRetryableFailureSchedulesRetry(t *testing.T) {
	q := &fakeQueue{next: pending(3)}
	s := &fakeSender{err: &provider.Error{StatusCode: 503, Retryable: true, Message: "down"}}
	if _, err := newTestWorker(q, s, fakeLimiter{}).ProcessOne(context.Background(), notification.ChannelSMS); err != nil {
		t.Fatal(err)
	}
	// Attempt 3 with base 2s: full delay 8s, lower jitter edge 4s.
	if want := fixedNow.Add(4 * time.Second); !q.retryAt.Equal(want) {
		t.Errorf("retry at %v, want %v", q.retryAt, want)
	}
	if q.attempt.StatusCode != 503 || q.attempt.Error == "" {
		t.Errorf("attempt not recorded: %+v", q.attempt)
	}
}

func TestProcessOneHonoursRetryAfter(t *testing.T) {
	q := &fakeQueue{next: pending(1)}
	s := &fakeSender{err: &provider.Error{StatusCode: 429, Retryable: true, RetryAfter: 45 * time.Second}}
	if _, err := newTestWorker(q, s, fakeLimiter{}).ProcessOne(context.Background(), notification.ChannelSMS); err != nil {
		t.Fatal(err)
	}
	if want := fixedNow.Add(45 * time.Second); !q.retryAt.Equal(want) {
		t.Errorf("retry at %v, want %v (Retry-After beats the shorter backoff)", q.retryAt, want)
	}
}

func TestProcessOnePermanentFailure(t *testing.T) {
	q := &fakeQueue{next: pending(1)}
	s := &fakeSender{err: &provider.Error{StatusCode: 400, Retryable: false, Message: "invalid number"}}
	if _, err := newTestWorker(q, s, fakeLimiter{}).ProcessOne(context.Background(), notification.ChannelSMS); err != nil {
		t.Fatal(err)
	}
	if !q.failed || !q.retryAt.IsZero() {
		t.Errorf("a 400 must fail immediately without retry: failed=%v retryAt=%v", q.failed, q.retryAt)
	}
}

func TestProcessOneFailsAfterLastAttempt(t *testing.T) {
	q := &fakeQueue{next: pending(5)} // MaxAttempts is 5
	s := &fakeSender{err: &provider.Error{StatusCode: 500, Retryable: true}}
	if _, err := newTestWorker(q, s, fakeLimiter{}).ProcessOne(context.Background(), notification.ChannelSMS); err != nil {
		t.Fatal(err)
	}
	if !q.failed {
		t.Error("expected failure after the last attempt")
	}
}

func TestProcessOneReleasesWhenLimiterFails(t *testing.T) {
	q, s := &fakeQueue{next: pending(1)}, &fakeSender{}
	_, err := newTestWorker(q, s, fakeLimiter{err: errors.New("redis down")}).ProcessOne(context.Background(), notification.ChannelSMS)
	if err == nil {
		t.Error("expected the limiter error to be returned")
	}
	if !q.released || s.calls != 0 {
		t.Errorf("must release without sending: released=%v sends=%d", q.released, s.calls)
	}
}

func TestProcessOneShutdownWhileWaitingIsNotAnError(t *testing.T) {
	q, s := &fakeQueue{next: pending(1)}, &fakeSender{}
	_, err := newTestWorker(q, s, fakeLimiter{err: context.Canceled}).ProcessOne(context.Background(), notification.ChannelSMS)
	if err != nil || !q.released || s.calls != 0 {
		t.Errorf("err=%v released=%v sends=%d", err, q.released, s.calls)
	}
}

func TestProcessOneLostLockIsNotAnError(t *testing.T) {
	q := &fakeQueue{next: pending(1), recordFn: func() error { return notification.ErrLockLost }}
	if _, err := newTestWorker(q, &fakeSender{}, fakeLimiter{}).ProcessOne(context.Background(), notification.ChannelSMS); err != nil {
		t.Errorf("lost lock should be logged, not returned: %v", err)
	}
}
