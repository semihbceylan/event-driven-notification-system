// Command worker claims pending notifications from the queue and delivers
// them through the external provider.
//
// Step 1 only wires up dependencies; the processing loop comes in step 4.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/semihbceylan/event-driven-notification-system/internal/config"
	"github.com/semihbceylan/event-driven-notification-system/internal/database"
	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
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

	logger.Info("worker started", "workers_per_channel", cfg.WorkersPerChannel, "rate_limit_per_second", cfg.RateLimitPerSecond)
	<-ctx.Done()
	logger.Info("shutting down")
	return nil
}
