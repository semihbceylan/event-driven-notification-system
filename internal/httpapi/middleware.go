package httpapi

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
	"github.com/semihbceylan/event-driven-notification-system/internal/metrics"
	"github.com/semihbceylan/event-driven-notification-system/internal/tracing"
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

// Hijack hands the raw connection to the WebSocket library, which checks
// for http.Hijacker directly instead of going through Unwrap.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(r.ResponseWriter).Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
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
			span := trace.SpanFromContext(r.Context())
			if traceID := tracing.TraceID(r.Context()); traceID != "" {
				logger = logger.With("trace_id", traceID)
				span.SetAttributes(attribute.String("correlation_id", id))
			}
			ctx := logging.WithCorrelationID(r.Context(), id)
			ctx = logging.WithLogger(ctx, logger)

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			req := r.WithContext(ctx)
			next.ServeHTTP(rec, req)
			// The mux stores the matched route pattern on the request it
			// was given, so read it from req, not r.
			elapsed := time.Since(start)
			if req.Pattern != "" {
				span.SetName(req.Pattern) // e.g. "POST /v1/notifications/batch"
				span.SetAttributes(semconv.HTTPRoute(req.Pattern))
			}

			if m != nil {
				m.Observe(r.Method, req.Pattern, rec.status, elapsed)
			}
			logger.Log(req.Context(), requestLogLevel(req.Pattern, rec.status), "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"route", req.Pattern,
				"status", rec.status,
				"duration_ms", elapsed.Milliseconds(),
			)
		})
	}
}

// pollingRoutes are called every few seconds by health checks, Prometheus
// and the dashboard. Logged at info they made up almost 90% of the API's
// log lines and buried the requests that matter, so a successful call to
// one of them is logged at debug (LOG_LEVEL=debug shows them).
var pollingRoutes = map[string]bool{
	"GET /healthz":     true,
	"GET /readyz":      true,
	"GET /metrics":     true,
	"GET /v1/metrics":  true,
	"GET /v1/provider": true,
}

// requestLogLevel: server errors at error, client errors at warn, polling
// at debug, everything else at info.
func requestLogLevel(route string, status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case status >= 400:
		return slog.LevelWarn
	case pollingRoutes[route]:
		return slog.LevelDebug
	default:
		return slog.LevelInfo
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
