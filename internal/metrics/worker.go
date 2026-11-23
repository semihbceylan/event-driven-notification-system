// Package metrics defines the Prometheus metrics exported by the API and the
// worker. Each process serves its own metrics at /metrics.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Worker holds the delivery metrics. Counters only go up; Prometheus derives
// rates from them, e.g. success rate over 5 minutes:
//
//	sum(rate(notifications_processed_total{outcome="sent"}[5m]))
//	  / sum(rate(notifications_processed_total[5m]))
type Worker struct {
	processed       *prometheus.CounterVec
	providerLatency *prometheus.HistogramVec
	deliveryLatency *prometheus.HistogramVec
	rateLimitWait   *prometheus.HistogramVec
	breakerState    *prometheus.GaugeVec
}

func NewWorker(reg prometheus.Registerer) *Worker {
	m := &Worker{
		processed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifications_processed_total",
			Help: "Delivery attempts by result: sent, retry (will be tried again) or failed (gave up).",
		}, []string{"channel", "outcome"}),
		providerLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "provider_request_duration_seconds",
			Help:    "Duration of provider HTTP calls.",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"channel", "result"}),
		deliveryLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "notification_delivery_latency_seconds",
			Help:    "Time from when a notification became due (created or scheduled) until it was sent.",
			Buckets: []float64{.1, .5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600},
		}, []string{"channel", "priority"}),
		rateLimitWait: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rate_limit_wait_seconds",
			Help:    "Time spent waiting for a rate-limit slot before sending.",
			Buckets: []float64{0, .005, .01, .05, .1, .25, .5, 1},
		}, []string{"channel"}),
		breakerState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "circuit_breaker_state",
			Help: "Circuit breaker state per channel: 0 closed, 1 open, 2 half-open.",
		}, []string{"channel"}),
	}
	reg.MustRegister(m.processed, m.providerLatency, m.deliveryLatency, m.rateLimitWait, m.breakerState)
	return m
}

func (m *Worker) Processed(channel, outcome string) {
	m.processed.WithLabelValues(channel, outcome).Inc()
}

// ProviderCall records one provider request; result is success,
// retryable_error or permanent_error.
func (m *Worker) ProviderCall(channel, result string, d time.Duration) {
	m.providerLatency.WithLabelValues(channel, result).Observe(d.Seconds())
}

func (m *Worker) Delivered(channel, priority string, sinceDue time.Duration) {
	m.deliveryLatency.WithLabelValues(channel, priority).Observe(sinceDue.Seconds())
}

func (m *Worker) RateLimitWait(channel string, d time.Duration) {
	m.rateLimitWait.WithLabelValues(channel).Observe(d.Seconds())
}

func (m *Worker) BreakerState(channel string, state int) {
	m.breakerState.WithLabelValues(channel).Set(float64(state))
}
