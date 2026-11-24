package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

type StatsSource interface {
	Stats(ctx context.Context, window time.Duration) (map[notification.Channel]*notification.ChannelStats, error)
}

type MetricsHandler struct {
	Stats StatsSource
	// RateLimitPerSecond is the configured per-channel send limit, reported
	// so the dashboard can draw it next to the measured send rate.
	RateLimitPerSecond int
}

type metricsResponse struct {
	Window      string                                              `json:"window"`
	GeneratedAt time.Time                                           `json:"generated_at"`
	Channels    map[notification.Channel]*notification.ChannelStats `json:"channels"`
	RateLimit   int                                                 `json:"rate_limit_per_second"`
}

// Summary handles GET /v1/metrics?window=5m: queue depth, success and
// failure rates, throughput and latency per channel. It is the
// human-readable view; Prometheus scrapes /metrics for dashboards and alerts.
func (h MetricsHandler) Summary(w http.ResponseWriter, r *http.Request) {
	window := 5 * time.Minute
	if v := r.URL.Query().Get("window"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > 24*time.Hour {
			writeValidationError(w, []notification.FieldError{{Field: "window", Message: "must be a duration between 1m and 24h, e.g. 5m or 1h"}})
			return
		}
		window = d
	}

	stats, err := h.Stats.Stats(r.Context(), window)
	if err != nil {
		(&NotificationHandler{}).handleStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, metricsResponse{
		Window: window.String(), GeneratedAt: time.Now().UTC(), Channels: stats, RateLimit: h.RateLimitPerSecond,
	})
}
