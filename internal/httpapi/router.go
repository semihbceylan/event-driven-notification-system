package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/semihbceylan/event-driven-notification-system/internal/metrics"
)

// Deps are the handlers and services the router wires together. Optional
// fields may be nil, e.g. in tests.
type Deps struct {
	Logger        *slog.Logger
	Health        HealthHandler
	Notifications *NotificationHandler
	Metrics       *MetricsHandler // GET /v1/metrics
	Prometheus    http.Handler    // GET /metrics
	HTTPMetrics   *metrics.HTTP
}

// NewRouter wires every route and the shared middleware.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", d.Health.Live)
	mux.HandleFunc("GET /readyz", d.Health.Ready)

	n := d.Notifications
	mux.HandleFunc("POST /v1/notifications", n.Create)
	mux.HandleFunc("POST /v1/notifications/batch", n.CreateBatch)
	mux.HandleFunc("GET /v1/notifications", n.List)
	mux.HandleFunc("GET /v1/notifications/{id}", n.Get)
	mux.HandleFunc("POST /v1/notifications/{id}/cancel", n.Cancel)
	mux.HandleFunc("POST /v1/notifications/{id}/retry", n.Retry)
	mux.HandleFunc("GET /v1/batches/{id}", n.GetBatch)

	if d.Metrics != nil {
		mux.HandleFunc("GET /v1/metrics", d.Metrics.Summary)
	}
	if d.Prometheus != nil {
		mux.Handle("GET /metrics", d.Prometheus)
	}

	// Order matters: RequestContext runs first so that Recover can log
	// panics with the correlation ID.
	return RequestContext(d.Logger, d.HTTPMetrics)(Recover(mux))
}
