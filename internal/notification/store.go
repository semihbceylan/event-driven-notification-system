package notification

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("notification not found")

// InvalidStateError is returned when an action is not allowed in the
// notification's current status, e.g. cancelling one that was already sent.
type InvalidStateError struct {
	Action   string // "cancel" or "retry"
	Required Status // the status the action needs
	Current  Status
}

func (e *InvalidStateError) Error() string {
	return fmt.Sprintf("only %s notifications can be %s; this one is %s", e.Required, pastTense(e.Action), e.Current)
}

func pastTense(action string) string {
	switch action {
	case "cancel":
		return "cancelled"
	case "retry":
		return "retried"
	}
	return action + "ed"
}

// IdempotencyConflictError is returned when an idempotency key is reused for
// a different notification. Silently returning the old one would hide a
// client bug, so we reject it instead.
type IdempotencyConflictError struct {
	Key string
}

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("idempotency key %q was already used for a different notification", e.Key)
}

// CreateResult says whether a notification was newly created or whether an
// existing one was returned because of its idempotency key.
type CreateResult struct {
	Notification Notification
	Created      bool
}

type ListFilter struct {
	Status  Status
	Channel Channel
	From    *time.Time // inclusive, on created_at
	To      *time.Time // exclusive, on created_at
	Cursor  *Cursor
	Limit   int
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const columns = `id, batch_id, idempotency_key, correlation_id, channel, recipient, content,
	priority, status, attempts, max_attempts, next_attempt_at, provider_message_id,
	last_error, scheduled_at, sent_at, created_at, updated_at, template_name`

// ON CONFLICT ... DO NOTHING makes duplicate inserts a no-op instead of an
// error. If two requests with the same key race, Postgres makes the second
// one wait for the first to commit, so exactly one row is created.
const insertSQL = `
INSERT INTO notifications (id, batch_id, idempotency_key, correlation_id, channel, recipient,
	content, priority, status, max_attempts, next_attempt_at, scheduled_at, template_name)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
RETURNING ` + columns

func insertArgs(n Notification) []any {
	return []any{
		n.ID, n.BatchID, n.IdempotencyKey, n.CorrelationID, string(n.Channel), n.Recipient,
		n.Content, n.Priority.Rank(), string(n.Status), n.MaxAttempts, n.NextAttemptAt, n.ScheduledAt,
		n.TemplateName,
	}
}

// querier is satisfied by both the pool and a transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) Create(ctx context.Context, n Notification) (CreateResult, error) {
	created, err := scanNotification(s.pool.QueryRow(ctx, insertSQL, insertArgs(n)...))
	if err == nil {
		return CreateResult{Notification: created, Created: true}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CreateResult{}, fmt.Errorf("insert notification: %w", err)
	}
	// No row returned: the idempotency key already exists.
	existing, err := existingForKey(ctx, s.pool, n)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Notification: existing, Created: false}, nil
}

// CreateBatch inserts all notifications in one transaction, so a batch is
// stored completely or not at all. pgx.Batch sends all inserts in a single
// network round trip instead of one per row.
func (s *Store) CreateBatch(ctx context.Context, ns []Notification) ([]CreateResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit

	batch := &pgx.Batch{}
	for _, n := range ns {
		batch.Queue(insertSQL, insertArgs(n)...)
	}

	results := make([]CreateResult, len(ns))
	var conflicts []int
	br := tx.SendBatch(ctx, batch)
	for i := range ns {
		created, err := scanNotification(br.QueryRow())
		switch {
		case err == nil:
			results[i] = CreateResult{Notification: created, Created: true}
		case errors.Is(err, pgx.ErrNoRows):
			conflicts = append(conflicts, i)
		default:
			_ = br.Close()
			return nil, fmt.Errorf("insert notification %d: %w", i, err)
		}
	}
	if err := br.Close(); err != nil {
		return nil, fmt.Errorf("insert batch: %w", err)
	}

	for _, i := range conflicts {
		existing, err := existingForKey(ctx, tx, ns[i])
		if err != nil {
			return nil, err
		}
		results[i] = CreateResult{Notification: existing, Created: false}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return results, nil
}

// existingForKey loads the notification that already owns n's idempotency
// key and checks that it describes the same message.
func existingForKey(ctx context.Context, q querier, n Notification) (Notification, error) {
	existing, err := scanNotification(q.QueryRow(ctx,
		`SELECT `+columns+` FROM notifications WHERE idempotency_key = $1`, n.IdempotencyKey))
	if err != nil {
		return Notification{}, fmt.Errorf("load notification by idempotency key: %w", err)
	}
	if existing.Channel != n.Channel || existing.Recipient != n.Recipient || existing.Content != n.Content {
		return Notification{}, &IdempotencyConflictError{Key: *n.IdempotencyKey}
	}
	return existing, nil
}

func (s *Store) Get(ctx context.Context, id uuid.UUID) (Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `SELECT `+columns+` FROM notifications WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return n, nil
}

func (s *Store) ListByBatch(ctx context.Context, batchID uuid.UUID) ([]Notification, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+columns+` FROM notifications WHERE batch_id = $1 ORDER BY created_at, id`, batchID)
	if err != nil {
		return nil, fmt.Errorf("list batch: %w", err)
	}
	return collect(rows)
}

// Cancel moves a pending notification to cancelled. The status check is in
// the WHERE clause, so it is atomic: if a worker claims the row at the same
// moment, exactly one of the two wins.
func (s *Store) Cancel(ctx context.Context, id uuid.UUID) (Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `
		UPDATE notifications
		SET status = 'cancelled', updated_at = now()
		WHERE id = $1 AND status = 'pending'
		RETURNING `+columns, id))
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, fmt.Errorf("cancel notification: %w", err)
	}

	// Nothing updated: either the ID does not exist or it is not pending.
	current, err := s.Get(ctx, id)
	if err != nil {
		return Notification{}, err
	}
	return Notification{}, &InvalidStateError{Action: "cancel", Required: StatusPending, Current: current.Status}
}

// Retry puts a failed notification back in the queue, for example after a
// provider outage that lasted longer than the retry schedule. It gets
// extraAttempts more tries; attempts is not reset, so the numbering in
// delivery_attempts keeps going and the audit trail stays readable.
func (s *Store) Retry(ctx context.Context, id uuid.UUID, extraAttempts int) (Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `
		UPDATE notifications
		SET status = 'pending', max_attempts = attempts + $2, next_attempt_at = now(),
		    locked_until = NULL, updated_at = now()
		WHERE id = $1 AND status = 'failed'
		RETURNING `+columns, id, extraAttempts))
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, fmt.Errorf("retry notification: %w", err)
	}
	current, err := s.Get(ctx, id)
	if err != nil {
		return Notification{}, err
	}
	return Notification{}, &InvalidStateError{Action: "retry", Required: StatusFailed, Current: current.Status}
}

// List returns one page of notifications, newest first, and the cursor for
// the next page (nil on the last page).
func (s *Store) List(ctx context.Context, f ListFilter) ([]Notification, *Cursor, error) {
	var (
		where []string
		args  []any
	)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if f.Status != "" {
		where = append(where, "status = "+arg(string(f.Status)))
	}
	if f.Channel != "" {
		where = append(where, "channel = "+arg(string(f.Channel)))
	}
	if f.From != nil {
		where = append(where, "created_at >= "+arg(*f.From))
	}
	if f.To != nil {
		where = append(where, "created_at < "+arg(*f.To))
	}
	if f.Cursor != nil {
		where = append(where, fmt.Sprintf("(created_at, id) < (%s, %s)", arg(f.Cursor.CreatedAt), arg(f.Cursor.ID)))
	}

	query := `SELECT ` + columns + ` FROM notifications`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// Fetch one extra row to know whether there is a next page.
	query += " ORDER BY created_at DESC, id DESC LIMIT " + arg(f.Limit+1)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list notifications: %w", err)
	}
	items, err := collect(rows)
	if err != nil {
		return nil, nil, err
	}

	if len(items) <= f.Limit {
		return items, nil, nil
	}
	items = items[:f.Limit]
	last := items[len(items)-1]
	return items, &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}

func collect(rows pgx.Rows) ([]Notification, error) {
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return out, nil
}

// scanNotification reads one row in the order given by the columns constant.
func scanNotification(row pgx.Row) (Notification, error) {
	var (
		n       Notification
		channel string
		status  string
		rank    int16
	)
	err := row.Scan(
		&n.ID, &n.BatchID, &n.IdempotencyKey, &n.CorrelationID, &channel, &n.Recipient, &n.Content,
		&rank, &status, &n.Attempts, &n.MaxAttempts, &n.NextAttemptAt, &n.ProviderMessageID,
		&n.LastError, &n.ScheduledAt, &n.SentAt, &n.CreatedAt, &n.UpdatedAt, &n.TemplateName,
	)
	if err != nil {
		return Notification{}, err
	}
	n.Channel = Channel(channel)
	n.Status = Status(status)
	n.Priority = priorityFromRank(rank)
	return n, nil
}
