package httpapi

import (
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
	"github.com/semihbceylan/event-driven-notification-system/internal/metrics"
)

const CorrelationIDHeader = "X-Correlation-ID"

// Only accept IDs that are safe to put in logs and headers.
var validCorrelationID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// statusRecorder remembers the status code so we can log it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the original writer, which
// WebSocket upgrades and streaming responses need.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// RequestContext gives every request a correlation ID and a logger that
// includes it, then logs one line per request when it finishes and records
// request metrics (m may be nil).
func RequestContext(base *slog.Logger, m *metrics.HTTP) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			id := r.Header.Get(CorrelationIDHeader)
			if !validCorrelationID.MatchString(id) {
				id = uuid.NewString()
			}
			w.Header().Set(CorrelationIDHeader, id)

			logger := base.With("correlation_id", id)
			ctx := logging.WithCorrelationID(r.Context(), id)
			ctx = logging.WithLogger(ctx, logger)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			req := r.WithContext(ctx)
			next.ServeHTTP(rec, req)
			// The mux stores the matched route pattern on the request it
			// was given, so read it from req, not r.
			elapsed := time.Since(start)

			if m != nil {
				m.Observe(r.Method, req.Pattern, rec.status, elapsed)
			}
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"route", req.Pattern,
				"status", rec.status,
				"duration_ms", elapsed.Milliseconds(),
			)
		})
	}
}

// Recover turns a panic in a handler into a 500 response instead of
// crashing the whole server.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				logging.FromContext(r.Context()).Error("panic in handler", "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
