package notification

// Integration tests: these run the real SQL against a real PostgreSQL in a
// throwaway container (testcontainers), so they need Docker. They are
// skipped with -short.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/semihbceylan/event-driven-notification-system/internal/database"
)

// newTestStore starts Postgres once per test and applies the real migrations.
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test; needs Docker")
	}
	ctx := context.Background()

	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("notify"),
		postgres.WithUsername("notify"),
		postgres.WithPassword("notify"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })

	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool), pool
}

func newSMS(content, key string) Notification {
	in := CreateInput{Recipient: "+905551234567", Channel: ChannelSMS, Content: content, Priority: PriorityHigh, IdempotencyKey: key}
	return New(in, nil, "corr-test", 5, time.Now())
}

func TestStore(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	t.Run("create and get round trip", func(t *testing.T) {
		n := newSMS("hello", "")
		res, err := store.Create(ctx, n)
		if err != nil || !res.Created {
			t.Fatalf("create: %v, created=%v", err, res.Created)
		}
		got, err := store.Get(ctx, n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Priority != PriorityHigh || got.Channel != ChannelSMS || got.Status != StatusPending ||
			got.Content != "hello" || got.CorrelationID == nil || *got.CorrelationID != "corr-test" ||
			got.BatchID != nil || got.IdempotencyKey != nil {
			t.Errorf("unexpected row: %+v", got)
		}
	})

	t.Run("get missing", func(t *testing.T) {
		if _, err := store.Get(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("idempotent create", func(t *testing.T) {
		first, err := store.Create(ctx, newSMS("once", "key-1"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.Create(ctx, newSMS("once", "key-1"))
		if err != nil {
			t.Fatal(err)
		}
		if second.Created || second.Notification.ID != first.Notification.ID {
			t.Errorf("replay should return the original: first=%v second=%v created=%v",
				first.Notification.ID, second.Notification.ID, second.Created)
		}

		_, err = store.Create(ctx, newSMS("different", "key-1"))
		var conflict *IdempotencyConflictError
		if !errors.As(err, &conflict) {
			t.Errorf("reused key with other content: err = %v, want IdempotencyConflictError", err)
		}
	})

	t.Run("concurrent creates with same key make one row", func(t *testing.T) {
		const n = 20
		var wg sync.WaitGroup
		ids := make(chan uuid.UUID, n)
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := store.Create(ctx, newSMS("race", "key-race"))
				if err != nil {
					t.Error(err)
					return
				}
				ids <- res.Notification.ID
			}()
		}
		wg.Wait()
		close(ids)

		seen := map[uuid.UUID]bool{}
		for id := range ids {
			seen[id] = true
		}
		var rows int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE idempotency_key = 'key-race'`).Scan(&rows)
		if len(seen) != 1 || rows != 1 {
			t.Errorf("got %d distinct IDs and %d rows, want 1 and 1", len(seen), rows)
		}
	})

	t.Run("batch create, replay and list by batch", func(t *testing.T) {
		existing, err := store.Create(ctx, newSMS("already there", "batch-key-0"))
		if err != nil {
			t.Fatal(err)
		}

		batchID := uuid.Must(uuid.NewV7())
		ns := make([]Notification, 5)
		for i := range ns {
			ns[i] = newSMS(fmt.Sprintf("msg %d", i), "")
			ns[i].BatchID = &batchID
		}
		// Item 0 reuses a key from an earlier request: it is returned, not re-inserted.
		ns[0] = newSMS("already there", "batch-key-0")
		ns[0].BatchID = &batchID

		results, err := store.CreateBatch(ctx, ns)
		if err != nil {
			t.Fatal(err)
		}
		if results[0].Created || results[0].Notification.ID != existing.Notification.ID {
			t.Errorf("item 0 should be a replay of %v, got %+v", existing.Notification.ID, results[0])
		}
		for i := 1; i < len(results); i++ {
			if !results[i].Created {
				t.Errorf("item %d should be created", i)
			}
		}

		inBatch, err := store.ListByBatch(ctx, batchID)
		if err != nil {
			t.Fatal(err)
		}
		if len(inBatch) != 4 {
			t.Errorf("batch has %d rows, want 4 (the replayed one belongs to its original request)", len(inBatch))
		}
	})

	t.Run("batch is all or nothing", func(t *testing.T) {
		var before int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&before)

		if _, err := store.Create(ctx, newSMS("original", "batch-conflict")); err != nil {
			t.Fatal(err)
		}
		ns := []Notification{newSMS("new one", ""), newSMS("changed", "batch-conflict")}
		if _, err := store.CreateBatch(ctx, ns); err == nil {
			t.Fatal("expected an idempotency conflict")
		}

		var after int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM notifications`).Scan(&after)
		if after != before+1 { // only the single Create above
			t.Errorf("failed batch left rows behind: before=%d after=%d", before, after)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		n := newSMS("cancel me", "")
		if _, err := store.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		got, err := store.Cancel(ctx, n.ID)
		if err != nil || got.Status != StatusCancelled {
			t.Fatalf("cancel: %v, status=%s", err, got.Status)
		}

		_, err = store.Cancel(ctx, n.ID)
		var nc *NotCancellableError
		if !errors.As(err, &nc) || nc.Status != StatusCancelled {
			t.Errorf("second cancel: err = %v", err)
		}

		if _, err := store.Cancel(ctx, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Errorf("cancel missing: err = %v, want ErrNotFound", err)
		}
	})

	t.Run("cannot cancel while processing", func(t *testing.T) {
		n := newSMS("in flight", "")
		if _, err := store.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE notifications SET status = 'processing' WHERE id = $1`, n.ID); err != nil {
			t.Fatal(err)
		}
		_, err := store.Cancel(ctx, n.ID)
		var nc *NotCancellableError
		if !errors.As(err, &nc) || nc.Status != StatusProcessing {
			t.Errorf("err = %v, want NotCancellableError{processing}", err)
		}
	})
}

func TestStoreListPagination(t *testing.T) {
	store, pool := newTestStore(t)
	ctx := context.Background()

	// 7 email + 3 push, with distinct created_at values one minute apart.
	base := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for i := range 10 {
		ch, rcpt := ChannelEmail, fmt.Sprintf("u%d@example.com", i)
		if i%3 == 0 && i > 0 {
			ch, rcpt = ChannelPush, fmt.Sprintf("token-%d", i)
		}
		n := New(CreateInput{Recipient: rcpt, Channel: ch, Content: "x", Priority: PriorityNormal}, nil, "", 5, base)
		if _, err := store.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE notifications SET created_at = $1 WHERE id = $2`, base.Add(time.Duration(i)*time.Minute), n.ID); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("pages cover every row once, newest first", func(t *testing.T) {
		var all []Notification
		var cursor *Cursor
		pages := 0
		for {
			items, next, err := store.List(ctx, ListFilter{Limit: 3, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, items...)
			pages++
			if next == nil {
				break
			}
			cursor = next
		}
		if len(all) != 10 || pages != 4 {
			t.Fatalf("got %d rows in %d pages, want 10 in 4", len(all), pages)
		}
		seen := map[uuid.UUID]bool{}
		for i, n := range all {
			if seen[n.ID] {
				t.Errorf("row %v returned twice", n.ID)
			}
			seen[n.ID] = true
			if i > 0 && n.CreatedAt.After(all[i-1].CreatedAt) {
				t.Errorf("row %d is newer than row %d", i, i-1)
			}
		}
	})

	t.Run("filter by channel", func(t *testing.T) {
		items, next, err := store.List(ctx, ListFilter{Limit: 50, Channel: ChannelPush})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 3 || next != nil {
			t.Errorf("got %d push rows (next=%v), want 3", len(items), next)
		}
	})

	t.Run("filter by date range", func(t *testing.T) {
		from, to := base.Add(2*time.Minute), base.Add(5*time.Minute)
		items, _, err := store.List(ctx, ListFilter{Limit: 50, From: &from, To: &to})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 3 { // minutes 2, 3, 4: from is inclusive, to is exclusive
			t.Errorf("got %d rows, want 3", len(items))
		}
	})

	t.Run("filter by status", func(t *testing.T) {
		items, _, err := store.List(ctx, ListFilter{Limit: 50, Status: StatusSent})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Errorf("got %d sent rows, want 0", len(items))
		}
	})
}
