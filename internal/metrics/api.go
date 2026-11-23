package metrics

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// HTTP holds request metrics for the API.
type HTTP struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func NewHTTP(reg prometheus.Registerer) *HTTP {
	m := &HTTP{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by route pattern and status code.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration by route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	reg.MustRegister(m.requests, m.duration)
	return m
}

// Observe records one request. route must be the route pattern (for example
// "GET /v1/notifications/{id}"), never the raw path: raw paths contain IDs,
// and a label per ID would create millions of time series.
func (m *HTTP) Observe(method, route string, status int, d time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(d.Seconds())
}

// QueueDepth is one row of the queue depth gauge.
type QueueDepth struct {
	Channel                  string
	Due, Waiting, Processing int64
}

// QueueCollector reads the queue depth from the database each time
// Prometheus scrapes, so the value is always current and correct across
// replicas (a counter kept in memory would only see this process).
type QueueCollector struct {
	read   func(ctx context.Context) ([]QueueDepth, error)
	logger *slog.Logger
	desc   *prometheus.Desc
}

func NewQueueCollector(read func(ctx context.Context) ([]QueueDepth, error), logger *slog.Logger) *QueueCollector {
	return &QueueCollector{
		read:   read,
		logger: logger,
		desc: prometheus.NewDesc("notifications_queue_depth",
			"Notifications waiting per channel. state: due (ready now), waiting (scheduled or in retry backoff), processing.",
			[]string{"channel", "state"}, nil),
	}
}

func (c *QueueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *QueueCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rows, err := c.read(ctx)
	if err != nil {
		c.logger.Warn("could not read queue depth for metrics", "error", err)
		return
	}
	for _, r := range rows {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(r.Due), r.Channel, "due")
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(r.Waiting), r.Channel, "waiting")
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(r.Processing), r.Channel, "processing")
	}
}
