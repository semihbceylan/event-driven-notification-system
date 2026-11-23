// Command worker claims pending notifications from the queue and delivers
// them through the external provider. It also serves /healthz, /readyz and
// /metrics on WORKER_HTTP_ADDR.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/semihbceylan/event-driven-notification-system/internal/breaker"
	"github.com/semihbceylan/event-driven-notification-system/internal/config"
	"github.com/semihbceylan/event-driven-notification-system/internal/database"
	"github.com/semihbceylan/event-driven-notification-system/internal/events"
	"github.com/semihbceylan/event-driven-notification-system/internal/httpapi"
	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
	"github.com/semihbceylan/event-driven-notification-system/internal/metrics"
	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
	"github.com/semihbceylan/event-driven-notification-system/internal/provider"
	"github.com/semihbceylan/event-driven-notification-system/internal/ratelimit"
	"github.com/semihbceylan/event-driven-notification-system/internal/worker"
)

func main() {
	logger := logging.New("worker", os.Getenv("LOG_LEVEL"))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("worker stopped with error", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer func() { _ = rdb.Close() }()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	w := worker.New(
		worker.Config{
			Channels:          notification.Channels,
			WorkersPerChannel: cfg.WorkersPerChannel,
			PollInterval:      cfg.PollInterval,
			LockTimeout:       cfg.LockTimeout,
			RetryBaseDelay:    cfg.RetryBaseDelay,
			RetryMaxDelay:     cfg.RetryMaxDelay,
			ReapInterval:      cfg.ReapInterval,
			Breaker: breaker.Config{
				Threshold:   cfg.BreakerThreshold,
				Cooldown:    cfg.BreakerCooldown,
				MaxCooldown: cfg.BreakerMaxCooldown,
			},
		},
		notification.NewStore(pool),
		provider.NewClient(cfg.ProviderURL, cfg.ProviderTimeout),
		ratelimit.New(rdb, cfg.RateLimitPerSecond, time.Second),
		metrics.NewWorker(reg),
		logger,
	)

	w.SetEventPublisher(events.NewPublisher(rdb, logger))

	// Health and metrics. An open circuit breaker does not make the worker
	// unhealthy: it is doing its job by waiting, and restarting it would not
	// help. Its state is shown for information.
	health := httpapi.HealthHandler{Deps: map[string]httpapi.Pinger{
		"postgres": pool,
		"redis":    httpapi.PingFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", health.Live)
	mux.HandleFunc("GET /readyz", health.Ready)
	mux.HandleFunc("GET /breakers", func(rw http.ResponseWriter, _ *http.Request) {
		httpapi.WriteJSON(rw, http.StatusOK, w.BreakerStates())
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	srv := &http.Server{Addr: cfg.WorkerHTTPAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("worker http server stopped", "error", err)
		}
	}()

	logger.Info("worker started",
		"workers_per_channel", cfg.WorkersPerChannel,
		"rate_limit_per_second", cfg.RateLimitPerSecond,
		"provider_url", cfg.ProviderURL,
		"http_addr", cfg.WorkerHTTPAddr,
	)
	w.Run(ctx) // returns after SIGTERM once in-flight sends finish

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	logger.Info("worker stopped")
	return nil
}
