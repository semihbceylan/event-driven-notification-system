package httpapi

import (
	"log/slog"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

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
	// Order matters: otelhttp starts the request's span first, then
	// RequestContext adds the correlation ID (and names the span after the
	// route), then Recover can log panics with both.
	h := RequestContext(d.Logger, d.HTTPMetrics)(Recover(mux))
	return otelhttp.NewHandler(h, "http request",
		// Health checks, scrapes and the long-lived WebSocket are not
		// interesting traces.
		otelhttp.WithFilter(func(r *http.Request) bool {
			switch r.URL.Path {
			case "/healthz", "/readyz", "/metrics", "/v1/ws":
				return false
			}
			return true
		}),
	)
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
