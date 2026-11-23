// Package logging sets up structured JSON logging and carries the
// correlation ID and a request-scoped logger through context.Context.
//
// The correlation ID is created (or taken from the X-Correlation-ID header)
// when a request enters the API, stored on the notification row, and picked
// up again by the worker. Searching logs for one ID therefore shows the whole
// path of a notification, from API call to provider response.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

type ctxKey int

const (
	correlationIDKey ctxKey = iota
	loggerKey
)

// New returns a JSON logger at the given level (debug, info, warn, error).
func New(service, level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("service", service)
}

func WithCorrelationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}

func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, l)
}

// FromContext returns the request-scoped logger, or the default logger if
// none was set.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
