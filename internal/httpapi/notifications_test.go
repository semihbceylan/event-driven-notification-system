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
	srv := httptest.NewServer(NewRouter(Deps{Logger: logger, Notifications: h}))
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

// fakeTemplates is an in-memory TemplateRepository.
type fakeTemplates struct {
	mu      sync.Mutex
	byName  map[string]notification.Template
	lookups int
}

func (f *fakeTemplates) PutTemplate(_ context.Context, name string, in notification.TemplateInput) (notification.Template, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byName == nil {
		f.byName = map[string]notification.Template{}
	}
	_, existed := f.byName[name]
	t := notification.Template{Name: name, Channel: in.Channel, Content: in.Content, Variables: notification.Placeholders(in.Content)}
	f.byName[name] = t
	return t, !existed, nil
}

func (f *fakeTemplates) GetTemplate(_ context.Context, name string) (notification.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	t, ok := f.byName[name]
	if !ok {
		return notification.Template{}, notification.ErrTemplateNotFound
	}
	return t, nil
}

func (f *fakeTemplates) ListTemplates(context.Context) ([]notification.Template, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []notification.Template{}
	for _, t := range f.byName {
		out = append(out, t)
	}
	return out, nil
}

func (f *fakeTemplates) DeleteTemplate(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.byName[name]; !ok {
		return notification.ErrTemplateNotFound
	}
	delete(f.byName, name)
	return nil
}

func newTemplateServer(t *testing.T) (*httptest.Server, *fakeTemplates) {
	t.Helper()
	tpls := &fakeTemplates{}
	h := &NotificationHandler{Repo: newFakeRepo(), MaxAttempts: 5, Templates: tpls}
	srv := httptest.NewServer(NewRouter(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notifications: h,
		Templates:     &TemplateHandler{Repo: tpls},
	}))
	t.Cleanup(srv.Close)
	return srv, tpls
}

func TestTemplateCRUD(t *testing.T) {
	srv, _ := newTemplateServer(t)
	url := srv.URL + "/v1/templates/order_shipped"

	resp, body := do(t, http.MethodPut, url, `{"channel":"sms","content":"Hi {{name}}, order {{order_id}} shipped"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %v", resp.StatusCode, body)
	}
	if vars := body["variables"].([]any); len(vars) != 2 || vars[0] != "name" {
		t.Errorf("variables = %v", body["variables"])
	}
	resp, _ = do(t, http.MethodPut, url, `{"channel":"sms","content":"Hey {{name}}"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("replace: status %d, want 200", resp.StatusCode)
	}
	resp, body = do(t, http.MethodPut, srv.URL+"/v1/templates/Bad%20Name", `{"channel":"sms","content":"x"}`, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad name: %d %v", resp.StatusCode, body)
	}
	resp, body = do(t, http.MethodGet, srv.URL+"/v1/templates", "", nil)
	if resp.StatusCode != http.StatusOK || len(body["data"].([]any)) != 1 {
		t.Errorf("list: %d %v", resp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	dresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	dresp.Body.Close()
	if dresp.StatusCode != http.StatusNoContent {
		t.Errorf("delete: %d", dresp.StatusCode)
	}
	resp, _ = do(t, http.MethodGet, url, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("get deleted: %d", resp.StatusCode)
	}
}

func TestCreateFromTemplate(t *testing.T) {
	srv, tpls := newTemplateServer(t)
	do(t, http.MethodPut, srv.URL+"/v1/templates/shipped", `{"channel":"sms","content":"Hi {{name}}, order {{order_id}} shipped"}`, nil)

	resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications",
		`{"recipient":"+905551234567","channel":"sms","template":"shipped","variables":{"name":"Ayse","order_id":"42"}}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %v", resp.StatusCode, body)
	}
	if body["content"] != "Hi Ayse, order 42 shipped" || body["template"] != "shipped" {
		t.Errorf("content=%v template=%v", body["content"], body["template"])
	}

	cases := []struct {
		name, body, field string
	}{
		{"missing variable", `{"recipient":"+905551234567","channel":"sms","template":"shipped","variables":{"name":"A"}}`, "variables"},
		{"unknown template", `{"recipient":"+905551234567","channel":"sms","template":"nope"}`, "template"},
		{"wrong channel", `{"recipient":"a@example.com","channel":"email","template":"shipped","variables":{"name":"A","order_id":"1"}}`, "template"},
		{"content and template", `{"recipient":"+905551234567","channel":"sms","content":"x","template":"shipped","variables":{"name":"A","order_id":"1"}}`, "content"},
		{"variables without template", `{"recipient":"+905551234567","channel":"sms","content":"x","variables":{"a":"b"}}`, "variables"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := do(t, http.MethodPost, srv.URL+"/v1/notifications", tc.body, nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status %d: %v", resp.StatusCode, body)
			}
			details := body["error"].(map[string]any)["details"].([]any)
			if len(details) != 1 || details[0].(map[string]any)["field"] != tc.field {
				t.Errorf("details = %v, want one error on %q", details, tc.field)
			}
		})
	}

	// A batch looks each template up once, however many items use it.
	tpls.lookups = 0
	var items []string
	for i := range 50 {
		items = append(items, fmt.Sprintf(`{"recipient":"+90555%07d","channel":"sms","template":"shipped","variables":{"name":"U%d","order_id":"%d"}}`, i, i, i))
	}
	resp, body = do(t, http.MethodPost, srv.URL+"/v1/notifications/batch", `{"notifications":[`+strings.Join(items, ",")+`]}`, nil)
	if resp.StatusCode != http.StatusCreated || body["created"] != float64(50) {
		t.Fatalf("batch: %d %v", resp.StatusCode, body)
	}
	if tpls.lookups != 1 {
		t.Errorf("template looked up %d times, want 1", tpls.lookups)
	}

	// Errors in a batch are indexed.
	resp, body = do(t, http.MethodPost, srv.URL+"/v1/notifications/batch",
		`{"notifications":[{"recipient":"+905551234567","channel":"sms","content":"ok"},{"recipient":"+905551234567","channel":"sms","template":"shipped"}]}`, nil)
	details := body["error"].(map[string]any)["details"].([]any)
	if resp.StatusCode != http.StatusUnprocessableEntity || details[0].(map[string]any)["field"] != "notifications[1].variables" {
		t.Errorf("batch error: %d %v", resp.StatusCode, details)
	}
}
