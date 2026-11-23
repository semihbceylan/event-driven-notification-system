package httpapi

import (
	"log/slog"
	"net/http"
)

// NewRouter wires every route and the shared middleware.
func NewRouter(logger *slog.Logger, health HealthHandler, notifications *NotificationHandler) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)

	mux.HandleFunc("POST /v1/notifications", notifications.Create)
	mux.HandleFunc("POST /v1/notifications/batch", notifications.CreateBatch)
	mux.HandleFunc("GET /v1/notifications", notifications.List)
	mux.HandleFunc("GET /v1/notifications/{id}", notifications.Get)
	mux.HandleFunc("POST /v1/notifications/{id}/cancel", notifications.Cancel)
	mux.HandleFunc("GET /v1/batches/{id}", notifications.GetBatch)

	// Order matters: RequestContext runs first so that Recover can log
	// panics with the correlation ID.
	return RequestContext(logger)(Recover(mux))
}
