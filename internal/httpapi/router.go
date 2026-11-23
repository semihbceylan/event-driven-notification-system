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
	WS            *WSHandler // GET /v1/ws
	Templates     *TemplateHandler
}

// NewRouter wires every route and the shared middleware.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()
	for _, r := range routes(d) {
		mux.HandleFunc(r.pattern, r.handler)
	}
	// Order matters: RequestContext runs first so that Recover can log
	// panics with the correlation ID.
	return RequestContext(d.Logger, d.HTTPMetrics)(Recover(mux))
}

type route struct {
	pattern string
	handler http.HandlerFunc
	// documented routes must appear in api/openapi.yaml (checked by a test).
	documented bool
}

func routes(d Deps) []route {
	n := d.Notifications
	rs := []route{
		{"GET /healthz", d.Health.Live, true},
		{"GET /readyz", d.Health.Ready, true},

		{"POST /v1/notifications", n.Create, true},
		{"POST /v1/notifications/batch", n.CreateBatch, true},
		{"GET /v1/notifications", n.List, true},
		{"GET /v1/notifications/{id}", n.Get, true},
		{"POST /v1/notifications/{id}/cancel", n.Cancel, true},
		{"POST /v1/notifications/{id}/retry", n.Retry, true},
		{"GET /v1/batches/{id}", n.GetBatch, true},

		{"GET /openapi.yaml", serveOpenAPI, false},
		{"GET /docs", serveDocs, false},
	}
	if d.Metrics != nil {
		rs = append(rs, route{"GET /v1/metrics", d.Metrics.Summary, true})
	}
	if d.Templates != nil {
		rs = append(rs,
			route{"GET /v1/templates", d.Templates.List, true},
			route{"GET /v1/templates/{name}", d.Templates.Get, true},
			route{"PUT /v1/templates/{name}", d.Templates.Put, true},
			route{"DELETE /v1/templates/{name}", d.Templates.Delete, true},
		)
	}
	if d.WS != nil {
		rs = append(rs, route{"GET /v1/ws", d.WS.Serve, true})
	}
	if d.Prometheus != nil {
		rs = append(rs, route{"GET /metrics", d.Prometheus.ServeHTTP, true})
	}
	return rs
}
