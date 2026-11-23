// Command api serves the notification management HTTP API.
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

	"github.com/semihbceylan/event-driven-notification-system/internal/config"
	"github.com/semihbceylan/event-driven-notification-system/internal/database"
	"github.com/semihbceylan/event-driven-notification-system/internal/httpapi"
	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
	"github.com/semihbceylan/event-driven-notification-system/internal/metrics"
	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

func main() {
	logger := logging.New("api", os.Getenv("LOG_LEVEL"))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("api stopped with error", "error", err)
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

	if err := database.Migrate(ctx, pool); err != nil {
		return err
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer func() { _ = rdb.Close() }()

	health := httpapi.HealthHandler{Deps: map[string]httpapi.Pinger{
		"postgres": pool,
		"redis":    httpapi.PingFunc(func(ctx context.Context) error { return rdb.Ping(ctx).Err() }),
	}}
	store := notification.NewStore(pool)
	notifications := &httpapi.NotificationHandler{
		Repo:        store,
		MaxAttempts: cfg.MaxAttempts,
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.NewQueueCollector(func(ctx context.Context) ([]metrics.QueueDepth, error) {
			depths, err := store.QueueDepths(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]metrics.QueueDepth, 0, len(depths))
			for ch, q := range depths {
				out = append(out, metrics.QueueDepth{Channel: string(ch), Due: q.Due, Waiting: q.Waiting, Processing: q.Processing})
			}
			return out, nil
		}, logger),
	)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.NewRouter(httpapi.Deps{
			Logger:        logger,
			Health:        health,
			Notifications: notifications,
			Metrics:       &httpapi.MetricsHandler{Stats: store},
			Prometheus:    promhttp.HandlerFor(reg, promhttp.HandlerOpts{}),
			HTTPMetrics:   metrics.NewHTTP(reg),
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
