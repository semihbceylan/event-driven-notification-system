package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/semihbceylan/event-driven-notification-system/internal/events"
	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

const (
	IdempotencyKeyHeader = "Idempotency-Key"
	// A full batch of 1000 maximum-length emails would be larger than this,
	// but realistic batches are far smaller. This protects the server from
	// huge bodies.
	maxBodyBytes     = 10 << 20
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// Repository is the storage the handlers need. notification.Store implements
// it; tests can use a fake.
type Repository interface {
	Create(ctx context.Context, n notification.Notification) (notification.CreateResult, error)
	CreateBatch(ctx context.Context, ns []notification.Notification) ([]notification.CreateResult, error)
	Get(ctx context.Context, id uuid.UUID) (notification.Notification, error)
	ListByBatch(ctx context.Context, batchID uuid.UUID) ([]notification.Notification, error)
	Cancel(ctx context.Context, id uuid.UUID) (notification.Notification, error)
	Retry(ctx context.Context, id uuid.UUID, extraAttempts int) (notification.Notification, error)
	List(ctx context.Context, f notification.ListFilter) ([]notification.Notification, *notification.Cursor, error)
}

// EventPublisher broadcasts status changes to live clients. Optional.
type EventPublisher interface {
	Publish(ctx context.Context, evs ...events.Event)
}

type NotificationHandler struct {
	Repo        Repository
	MaxAttempts int
	Now         func() time.Time
	Events      EventPublisher
}

func (h *NotificationHandler) publish(ctx context.Context, ns ...notification.Notification) {
	if h.Events == nil || len(ns) == 0 {
		return
	}
	evs := make([]events.Event, len(ns))
	for i, n := range ns {
		evs[i] = events.FromNotification(n, h.now())
	}
	h.Events.Publish(ctx, evs...)
}

func (h *NotificationHandler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Create handles POST /v1/notifications.
//
// The idempotency key comes from the Idempotency-Key header (the common
// convention, used by e.g. Stripe). Repeating a request with the same key
// returns the original notification with 200 instead of 201, and nothing is
// sent twice.
func (h *NotificationHandler) Create(w http.ResponseWriter, r *http.Request) {
	var in notification.CreateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	in.IdempotencyKey = r.Header.Get(IdempotencyKeyHeader)
	in.Normalize()
	if errs := in.Validate("", h.now()); len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}

	n := notification.New(in, nil, logging.CorrelationID(r.Context()), h.MaxAttempts, h.now())
	res, err := h.Repo.Create(r.Context(), n)
	if err != nil {
		h.handleStoreError(w, r, err)
		return
	}

	status := http.StatusCreated
	if !res.Created {
		w.Header().Set("Idempotent-Replayed", "true")
		status = http.StatusOK
	} else {
		h.publish(r.Context(), res.Notification)
	}
	writeJSON(w, status, res.Notification)
}

type batchRequest struct {
	Notifications []notification.CreateInput `json:"notifications"`
}

type batchResponse struct {
	BatchID       uuid.UUID                   `json:"batch_id"`
	Total         int                         `json:"total"`
	Created       int                         `json:"created"`
	Replayed      int                         `json:"replayed"`
	Notifications []notification.Notification `json:"notifications"`
}

// CreateBatch handles POST /v1/notifications/batch. Each item can carry its
// own idempotency_key. Items replayed from an earlier request keep the
// batch_id of the request that created them.
func (h *NotificationHandler) CreateBatch(w http.ResponseWriter, r *http.Request) {
	var req batchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	for i := range req.Notifications {
		req.Notifications[i].Normalize()
	}
	if errs := notification.ValidateBatch(req.Notifications, h.now()); len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}

	batchID := uuid.Must(uuid.NewV7())
	correlationID := logging.CorrelationID(r.Context())
	now := h.now()
	ns := make([]notification.Notification, len(req.Notifications))
	for i, in := range req.Notifications {
		ns[i] = notification.New(in, &batchID, correlationID, h.MaxAttempts, now)
	}

	results, err := h.Repo.CreateBatch(r.Context(), ns)
	if err != nil {
		h.handleStoreError(w, r, err)
		return
	}

	resp := batchResponse{BatchID: batchID, Total: len(results), Notifications: make([]notification.Notification, len(results))}
	var created []notification.Notification
	for i, res := range results {
		resp.Notifications[i] = res.Notification
		if res.Created {
			resp.Created++
			created = append(created, res.Notification)
		} else {
			resp.Replayed++
		}
	}
	h.publish(r.Context(), created...)
	status := http.StatusCreated
	if resp.Created == 0 {
		status = http.StatusOK
	}
	writeJSON(w, status, resp)
}

// Get handles GET /v1/notifications/{id}.
func (h *NotificationHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	n, err := h.Repo.Get(r.Context(), id)
	if err != nil {
		h.handleStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, n)
}

type batchStatusResponse struct {
	BatchID       uuid.UUID                   `json:"batch_id"`
	Total         int                         `json:"total"`
	StatusCounts  map[notification.Status]int `json:"status_counts"`
	Notifications []notification.Notification `json:"notifications"`
}

// GetBatch handles GET /v1/batches/{id}.
func (h *NotificationHandler) GetBatch(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	ns, err := h.Repo.ListByBatch(r.Context(), id)
	if err != nil {
		h.handleStoreError(w, r, err)
		return
	}
	if len(ns) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "batch not found", nil)
		return
	}

	counts := make(map[notification.Status]int)
	for _, n := range ns {
		counts[n.Status]++
	}
	writeJSON(w, http.StatusOK, batchStatusResponse{BatchID: id, Total: len(ns), StatusCounts: counts, Notifications: ns})
}

// Cancel handles POST /v1/notifications/{id}/cancel. Only pending
// notifications can be cancelled; anything else returns 409 Conflict.
func (h *NotificationHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	n, err := h.Repo.Cancel(r.Context(), id)
	if err != nil {
		h.handleStoreError(w, r, err)
		return
	}
	h.publish(r.Context(), n)
	writeJSON(w, http.StatusOK, n)
}

// Retry handles POST /v1/notifications/{id}/retry. Only failed
// notifications can be retried; they get MaxAttempts more tries.
func (h *NotificationHandler) Retry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	n, err := h.Repo.Retry(r.Context(), id, h.MaxAttempts)
	if err != nil {
		h.handleStoreError(w, r, err)
		return
	}
	h.publish(r.Context(), n)
	writeJSON(w, http.StatusOK, n)
}

type listResponse struct {
	Data       []notification.Notification `json:"data"`
	NextCursor *string                     `json:"next_cursor"`
}

// List handles GET /v1/notifications?status=&channel=&from=&to=&limit=&cursor=
// from and to are RFC 3339 timestamps on created_at; from is inclusive and to
// is exclusive.
func (h *NotificationHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := notification.ListFilter{Limit: defaultPageLimit}
	var errs []notification.FieldError

	if s := q.Get("status"); s != "" {
		f.Status = notification.Status(s)
		if !f.Status.Valid() {
			errs = append(errs, notification.FieldError{Field: "status", Message: "must be one of: pending, processing, sent, failed, cancelled"})
		}
	}
	if c := q.Get("channel"); c != "" {
		f.Channel = notification.Channel(c)
		if !f.Channel.Valid() {
			errs = append(errs, notification.FieldError{Field: "channel", Message: "must be one of: sms, email, push"})
		}
	}
	for _, p := range []struct {
		name string
		dst  **time.Time
	}{{"from", &f.From}, {"to", &f.To}} {
		if v := q.Get(p.name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				errs = append(errs, notification.FieldError{Field: p.name, Message: "must be an RFC 3339 timestamp, e.g. 2026-11-23T00:00:00Z"})
				continue
			}
			*p.dst = &t
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		errs = append(errs, notification.FieldError{Field: "from", Message: "must be before to"})
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			errs = append(errs, notification.FieldError{Field: "limit", Message: fmt.Sprintf("must be between 1 and %d", maxPageLimit)})
		} else {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		c, err := notification.DecodeCursor(v)
		if err != nil {
			errs = append(errs, notification.FieldError{Field: "cursor", Message: "is invalid; use the next_cursor value from the previous page"})
		} else {
			f.Cursor = &c
		}
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}

	items, next, err := h.Repo.List(r.Context(), f)
	if err != nil {
		h.handleStoreError(w, r, err)
		return
	}
	resp := listResponse{Data: items}
	if resp.Data == nil {
		resp.Data = []notification.Notification{} // [] in JSON, not null
	}
	if next != nil {
		s := next.Encode()
		resp.NextCursor = &s
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleStoreError maps storage errors to HTTP responses. Unexpected errors
// are logged with the correlation ID and hidden from the client.
func (h *NotificationHandler) handleStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var invalidState *notification.InvalidStateError
	var conflict *notification.IdempotencyConflictError
	switch {
	case errors.Is(err, notification.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "notification not found", nil)
	case errors.As(err, &invalidState):
		code := "not_cancellable"
		if invalidState.Action == "retry" {
			code = "not_retryable"
		}
		writeError(w, http.StatusConflict, code, invalidState.Error(), nil)
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, "idempotency_key_reused", conflict.Error(), nil)
	default:
		logging.FromContext(r.Context()).Error("storage error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
	}
}

// decodeJSON reads a JSON body strictly: unknown fields are rejected so that
// typos like "reciptient" fail loudly instead of being ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("body must contain a single JSON object")
	}
	if err == nil {
		return true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body must be at most %d bytes", maxBodyBytes), nil)
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid_json", err.Error(), nil)
	return false
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_id", name+" must be a UUID", nil)
		return uuid.Nil, false
	}
	return id, true
}
