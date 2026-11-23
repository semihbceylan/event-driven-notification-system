package notification

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrLockLost means a worker tried to record the result of a notification it
// no longer owns. This happens when its lock expired (for example after a
// long pause), the reaper put the row back in the queue, and another worker
// claimed it again. The late result is dropped instead of overwriting the
// newer attempt.
var ErrLockLost = errors.New("notification lock lost")

// Attempt is the outcome of one delivery try, stored in delivery_attempts.
type Attempt struct {
	StartedAt  time.Time
	StatusCode int // 0 when no HTTP response was received (timeout, network error)
	Error      string
	Duration   time.Duration
}

// claimSQL picks the next due notification for a channel and marks it as
// processing in one statement.
//
//   - FOR UPDATE SKIP LOCKED: if another worker is claiming a row right now,
//     skip it and take the next one instead of waiting. This is what lets
//     many workers share one table as a queue.
//   - ORDER BY priority, next_attempt_at: high priority first, then oldest.
//     It matches notifications_claim_idx, so this is an index scan.
//   - attempts is incremented here, and the new value acts as a fencing
//     token: results are only recorded if attempts still matches (see finish).
const claimSQL = `
UPDATE notifications
SET status = 'processing',
    attempts = attempts + 1,
    locked_until = now() + make_interval(secs => $2),
    updated_at = now()
WHERE id = (
    SELECT id FROM notifications
    WHERE status = 'pending' AND channel = $1 AND next_attempt_at <= now()
    ORDER BY priority, next_attempt_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING ` + columns

// Claim takes the next due notification on a channel. ok is false when the
// queue has nothing due.
func (s *Store) Claim(ctx context.Context, ch Channel, lockFor time.Duration) (n Notification, ok bool, err error) {
	n, err = scanNotification(s.pool.QueryRow(ctx, claimSQL, string(ch), lockFor.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, false, nil
	}
	if err != nil {
		return Notification{}, false, fmt.Errorf("claim: %w", err)
	}
	return n, true, nil
}

func (s *Store) MarkSent(ctx context.Context, n Notification, providerMessageID string, a Attempt) error {
	return s.finish(ctx, n, a, `
		UPDATE notifications
		SET status = 'sent', sent_at = now(), provider_message_id = NULLIF($3, ''),
		    last_error = NULL, locked_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		providerMessageID)
}

// MarkRetry puts the notification back in the queue, to be picked up again
// at nextAttemptAt.
func (s *Store) MarkRetry(ctx context.Context, n Notification, nextAttemptAt time.Time, a Attempt) error {
	return s.finish(ctx, n, a, `
		UPDATE notifications
		SET status = 'pending', next_attempt_at = $3, last_error = $4,
		    locked_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		nextAttemptAt, a.Error)
}

func (s *Store) MarkFailed(ctx context.Context, n Notification, a Attempt) error {
	return s.finish(ctx, n, a, `
		UPDATE notifications
		SET status = 'failed', last_error = $3, locked_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		a.Error)
}

// Release returns a claimed notification to the queue without counting an
// attempt. Used when the worker shuts down before it could send.
func (s *Store) Release(ctx context.Context, n Notification) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = 'pending', attempts = attempts - 1, locked_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'processing' AND attempts = $2`,
		n.ID, n.Attempts)
	if err != nil {
		return fmt.Errorf("release: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLockLost
	}
	return nil
}

// finish updates the notification and records the attempt in one
// transaction, so the status and the audit trail never disagree.
func (s *Store) finish(ctx context.Context, n Notification, a Attempt, updateSQL string, extra ...any) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	args := append([]any{n.ID, n.Attempts}, extra...)
	tag, err := tx.Exec(ctx, updateSQL, args...)
	if err != nil {
		return fmt.Errorf("update notification: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLockLost
	}

	var statusCode *int
	if a.StatusCode != 0 {
		statusCode = &a.StatusCode
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO delivery_attempts (notification_id, attempt_number, status_code, error, duration_ms, started_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, COALESCE($6, now()))`,
		n.ID, n.Attempts, statusCode, a.Error, a.Duration.Milliseconds(), nullTime(a.StartedAt)); err != nil {
		return fmt.Errorf("insert attempt: %w", err)
	}
	return tx.Commit(ctx)
}

// RequeueStuck recovers notifications whose worker died mid-send: any row
// still processing after its lock expired goes back to pending, or to failed
// if it has used up its attempts.
//
// This is why delivery is at-least-once: if the worker crashed after the
// provider accepted the message but before saving the result, the message
// is sent again. Passing the notification ID as Idempotency-Key lets a
// provider that supports it drop the duplicate.
func (s *Store) RequeueStuck(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications
		SET status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
		    last_error = 'worker lock expired before the result was recorded',
		    next_attempt_at = now(), locked_until = NULL, updated_at = now()
		WHERE status = 'processing' AND locked_until < now()`)
	if err != nil {
		return 0, fmt.Errorf("requeue stuck: %w", err)
	}
	return tag.RowsAffected(), nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
