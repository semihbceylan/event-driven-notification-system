package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

// fakeRepo is an in-memory Repository so handler tests run without a
// database. The real SQL is covered by the store's integration tests.
type fakeRepo struct {
	mu    sync.Mutex
	byID  map[uuid.UUID]notification.Notification
	byKey map[string]uuid.UUID
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byID: map[uuid.UUID]notification.Notification{}, byKey: map[string]uuid.UUID{}}
}

func (f *fakeRepo) create(n notification.Notification) (notification.CreateResult, error) {
	if n.IdempotencyKey != nil {
		if id, ok := f.byKey[*n.IdempotencyKey]; ok {
			existing := f.byID[id]
			if existing.Content != n.Content || existing.Recipient != n.Recipient || existing.Channel != n.Channel {
				return notification.CreateResult{}, &notification.IdempotencyConflictError{Key: *n.IdempotencyKey}
			}
			return notification.CreateResult{Notification: existing}, nil
		}
		f.byKey[*n.IdempotencyKey] = n.ID
	}
	n.CreatedAt = time.Now()
	f.byID[n.ID] = n
	return notification.CreateResult{Notification: n, Created: true}, nil
}

func (f *fakeRepo) Create(_ context.Context, n notification.Notification) (notification.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.create(n)
}

func (f *fakeRepo) CreateBatch(_ context.Context, ns []notification.Notification) ([]notification.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]notification.CreateResult, len(ns))
	for i, n := range ns {
		res, err := f.create(n)
		if err != nil {
			return nil, err
		}
		out[i] = res
	}
	return out, nil
}

func (f *fakeRepo) Get(_ context.Context, id uuid.UUID) (notification.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.byID[id]
	if !ok {
		return notification.Notification{}, notification.ErrNotFound
	}
	return n, nil
}

func (f *fakeRepo) ListByBatch(_ context.Context, batchID uuid.UUID) ([]notification.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []notification.Notification
	for _, n := range f.byID {
		if n.BatchID != nil && *n.BatchID == batchID {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeRepo) Cancel(_ context.Context, id uuid.UUID) (notification.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.byID[id]
	if !ok {
		return notification.Notification{}, notification.ErrNotFound
	}
	if n.Status != notification.StatusPending {
		return notification.Notification{}, &notification.InvalidStateError{Action: "cancel", Required: notification.StatusPending, Current: n.Status}
	}
	n.Status = notification.StatusCancelled
	f.byID[id] = n
	return n, nil
}

func (f *fakeRepo) Retry(_ context.Context, id uuid.UUID, extra int) (notification.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.byID[id]
	if !ok {
		return notification.Notification{}, notification.ErrNotFound
	}
	if n.Status != notification.StatusFailed {
		return notification.Notification{}, &notification.InvalidStateError{Action: "retry", Required: notification.StatusFailed, Current: n.Status}
	}
	n.Status = notification.StatusPending
	n.MaxAttempts = n.Attempts + extra
	f.byID[id] = n
	return n, nil
}

func (f *fakeRepo) List(_ context.Context, filter notification.ListFilter) ([]notification.Notification, *notification.Cursor, error) {
	return nil, nil, nil
}

func newTestServer(t *testing.T) (*httptest.Server, *fakeRepo) {
	t.Helper()
	repo := newFakeRepo()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &NotificationHandler{Repo: repo, MaxAttempts: 5}
	srv := httptest.NewServer(NewRouter(logger, HealthHandler{}, h))
	t.Cleanup(srv.Close)
	return srv, repo
}

func do(t *testing.T, method, url, body string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp, out
}

const smsBody = `{"recipient":"+905551234567","channel":"sms","content":"Flash sale!","priority":"high"}`

func TestCreateNotification(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications", smsBody, map[string]string{CorrelationIDHeader: "test-corr-1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["status"] != "pending" || body["priority"] != "high" {
		t.Errorf("unexpected body: %v", body)
	}
	if body["correlation_id"] != "test-corr-1" || resp.Header.Get(CorrelationIDHeader) != "test-corr-1" {
		t.Errorf("correlation ID not propagated: body=%v header=%q", body["correlation_id"], resp.Header.Get(CorrelationIDHeader))
	}
}

func TestCreateGeneratesCorrelationID(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, _ := do(t, http.MethodPost, srv.URL+"/v1/notifications", smsBody, nil)
	if _, err := uuid.Parse(resp.Header.Get(CorrelationIDHeader)); err != nil {
		t.Errorf("expected a generated UUID correlation ID, got %q", resp.Header.Get(CorrelationIDHeader))
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	srv, _ := newTestServer(t)
	headers := map[string]string{IdempotencyKeyHeader: "order-42-shipped"}

	first, firstBody := do(t, http.MethodPost, srv.URL+"/v1/notifications", smsBody, headers)
	second, secondBody := do(t, http.MethodPost, srv.URL+"/v1/notifications", smsBody, headers)

	if first.StatusCode != http.StatusCreated || second.StatusCode != http.StatusOK {
		t.Fatalf("statuses = %d, %d; want 201, 200", first.StatusCode, second.StatusCode)
	}
	if firstBody["id"] != secondBody["id"] {
		t.Errorf("replay returned a different notification: %v vs %v", firstBody["id"], secondBody["id"])
	}
	if second.Header.Get("Idempotent-Replayed") != "true" {
		t.Error("missing Idempotent-Replayed header on replay")
	}

	// Same key, different content: a client bug, so reject it.
	other := strings.Replace(smsBody, "Flash sale!", "Something else", 1)
	resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications", other, headers)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("reused key with new content: status = %d, body = %v", resp.StatusCode, body)
	}
}

func TestCreateValidation(t *testing.T) {
	srv, _ := newTestServer(t)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"invalid json", `{"recipient":`, http.StatusBadRequest, "invalid_json"},
		{"unknown field", `{"recipient":"+905551234567","channel":"sms","content":"x","reciptient":"typo"}`, http.StatusBadRequest, "invalid_json"},
		{"bad recipient", `{"recipient":"123","channel":"sms","content":"x"}`, http.StatusUnprocessableEntity, "validation_failed"},
		{"missing everything", `{}`, http.StatusUnprocessableEntity, "validation_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications", tt.body, nil)
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %v)", resp.StatusCode, tt.wantStatus, body)
			}
			errObj, _ := body["error"].(map[string]any)
			if errObj["code"] != tt.wantCode {
				t.Errorf("code = %v, want %s", errObj["code"], tt.wantCode)
			}
		})
	}
}

func TestBatchCreateAndStatus(t *testing.T) {
	srv, _ := newTestServer(t)

	var items []string
	for i := range 3 {
		items = append(items, fmt.Sprintf(`{"recipient":"user%d@example.com","channel":"email","content":"Hi %d"}`, i, i))
	}
	resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications/batch", `{"notifications":[`+strings.Join(items, ",")+`]}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, body)
	}
	if body["total"] != float64(3) || body["created"] != float64(3) {
		t.Errorf("unexpected counts: %v", body)
	}

	batchID := body["batch_id"].(string)
	resp, body = do(t, http.MethodGet, srv.URL+"/v1/batches/"+batchID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get batch status = %d", resp.StatusCode)
	}
	counts := body["status_counts"].(map[string]any)
	if counts["pending"] != float64(3) {
		t.Errorf("status_counts = %v", counts)
	}
}

func TestBatchRejectsInvalidItem(t *testing.T) {
	srv, repo := newTestServer(t)
	body := `{"notifications":[{"recipient":"+905551234567","channel":"sms","content":"ok"},{"recipient":"bad","channel":"sms","content":"x"}]}`

	resp, out := do(t, http.MethodPost, srv.URL+"/v1/notifications/batch", body, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %v", resp.StatusCode, out)
	}
	if len(repo.byID) != 0 {
		t.Errorf("an invalid batch must store nothing, stored %d", len(repo.byID))
	}
}

func TestCancel(t *testing.T) {
	srv, repo := newTestServer(t)
	_, created := do(t, http.MethodPost, srv.URL+"/v1/notifications", smsBody, nil)
	id := created["id"].(string)

	resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications/"+id+"/cancel", "", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "cancelled" {
		t.Fatalf("cancel: status = %d, body = %v", resp.StatusCode, body)
	}

	// Cancelling again is a conflict: it is no longer pending.
	resp, _ = do(t, http.MethodPost, srv.URL+"/v1/notifications/"+id+"/cancel", "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second cancel: status = %d, want 409", resp.StatusCode)
	}

	// A sent notification cannot be cancelled either.
	sent := repo.byID[uuid.MustParse(id)]
	sent.Status = notification.StatusSent
	repo.byID[sent.ID] = sent
	resp, _ = do(t, http.MethodPost, srv.URL+"/v1/notifications/"+id+"/cancel", "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cancel sent: status = %d, want 409", resp.StatusCode)
	}
}

func TestGetErrors(t *testing.T) {
	srv, _ := newTestServer(t)

	resp, _ := do(t, http.MethodGet, srv.URL+"/v1/notifications/not-a-uuid", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad id: status = %d, want 400", resp.StatusCode)
	}
	resp, _ = do(t, http.MethodGet, srv.URL+"/v1/notifications/"+uuid.NewString(), "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing id: status = %d, want 404", resp.StatusCode)
	}
}

func TestListValidatesQuery(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, q := range []string{"status=done", "channel=fax", "limit=0", "limit=1000", "from=yesterday", "cursor=xyz",
		"from=2026-11-24T00:00:00Z&to=2026-11-23T00:00:00Z"} {
		resp, _ := do(t, http.MethodGet, srv.URL+"/v1/notifications?"+q, "", nil)
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422", q, resp.StatusCode)
		}
	}

	resp, body := do(t, http.MethodGet, srv.URL+"/v1/notifications?status=pending&channel=sms&limit=10", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid list: status = %d", resp.StatusCode)
	}
	if data, ok := body["data"].([]any); !ok || len(data) != 0 {
		t.Errorf("empty list must be [], got %v", body["data"])
	}
}

func TestRetry(t *testing.T) {
	srv, repo := newTestServer(t)
	_, created := do(t, http.MethodPost, srv.URL+"/v1/notifications", smsBody, nil)
	id := created["id"].(string)

	resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications/"+id+"/retry", "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("retry pending: status = %d, body = %v", resp.StatusCode, body)
	}
	if errObj := body["error"].(map[string]any); errObj["code"] != "not_retryable" {
		t.Errorf("code = %v", errObj["code"])
	}

	n := repo.byID[uuid.MustParse(id)]
	n.Status, n.Attempts = notification.StatusFailed, 5
	repo.byID[n.ID] = n

	resp, body = do(t, http.MethodPost, srv.URL+"/v1/notifications/"+id+"/retry", "", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "pending" || body["max_attempts"] != float64(10) {
		t.Errorf("retry failed: status = %d, body = %v", resp.StatusCode, body)
	}
}
