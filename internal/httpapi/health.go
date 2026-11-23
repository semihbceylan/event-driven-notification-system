// Package httpapi contains the HTTP handlers of the notification API.
package httpapi

import (
	"context"
	"net/http"
	"time"
)

// Pinger is anything whose availability we can check, such as the
// database pool or the Redis client.
type Pinger interface {
	Ping(ctx context.Context) error
}

// PingFunc adapts a plain function to the Pinger interface.
type PingFunc func(ctx context.Context) error

func (f PingFunc) Ping(ctx context.Context) error { return f(ctx) }

// HealthHandler serves liveness (/healthz) and readiness (/readyz) checks.
// Liveness only says the process is up. Readiness also checks that every
// dependency is reachable, so a load balancer stops sending traffic when
// the database or Redis is down.
type HealthHandler struct {
	Deps map[string]Pinger
}

func (h HealthHandler) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	checks := make(map[string]string, len(h.Deps))
	status := http.StatusOK
	for name, dep := range h.Deps {
		if err := dep.Ping(ctx); err != nil {
			checks[name] = err.Error()
			status = http.StatusServiceUnavailable
			continue
		}
		checks[name] = "ok"
	}

	overall := "ok"
	if status != http.StatusOK {
		overall = "unavailable"
	}
	writeJSON(w, status, map[string]any{"status": overall, "checks": checks})
}
