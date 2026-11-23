// Command worker claims pending notifications from the queue and delivers
// them through the external provider.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/semihbceylan/event-driven-notification-system/internal/config"
	"github.com/semihbceylan/event-driven-notification-system/internal/database"
	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
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

	w := worker.New(
		worker.Config{
			Channels:          notification.Channels,
			WorkersPerChannel: cfg.WorkersPerChannel,
			PollInterval:      cfg.PollInterval,
			LockTimeout:       cfg.LockTimeout,
			RetryBaseDelay:    cfg.RetryBaseDelay,
			RetryMaxDelay:     cfg.RetryMaxDelay,
			ReapInterval:      cfg.ReapInterval,
		},
		notification.NewStore(pool),
		provider.NewClient(cfg.ProviderURL, cfg.ProviderTimeout),
		ratelimit.New(rdb, cfg.RateLimitPerSecond, time.Second),
		logger,
	)

	logger.Info("worker started",
		"workers_per_channel", cfg.WorkersPerChannel,
		"rate_limit_per_second", cfg.RateLimitPerSecond,
		"provider_url", cfg.ProviderURL,
	)
	w.Run(ctx) // returns after SIGTERM once in-flight sends finish
	logger.Info("worker stopped")
	return nil
}
