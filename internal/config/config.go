// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr    string
	DatabaseURL string
	RedisAddr   string
	LogLevel    string

	ProviderURL     string
	ProviderTimeout time.Duration

	// RateLimitPerSecond is the maximum number of messages sent per second
	// for each channel, shared across all worker replicas.
	RateLimitPerSecond int
	// WorkersPerChannel is the number of concurrent senders per channel
	// inside one worker process.
	WorkersPerChannel int
	// MaxAttempts is how many times delivery is tried before a
	// notification is marked failed.
	MaxAttempts int

	PollInterval   time.Duration
	LockTimeout    time.Duration
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	ReapInterval   time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:    getEnv("HTTP_ADDR", ":8080"),
		DatabaseURL: getEnv("DATABASE_URL", "postgres://notify:notify@localhost:5432/notify?sslmode=disable"),
		RedisAddr:   getEnv("REDIS_ADDR", "localhost:6379"),
		LogLevel:    getEnv("LOG_LEVEL", "info"),
		ProviderURL: getEnv("PROVIDER_URL", "http://localhost:8090/send"),
	}

	var err error
	if cfg.ProviderTimeout, err = getDuration("PROVIDER_TIMEOUT", 5*time.Second); err != nil {
		return Config{}, err
	}
	if cfg.RateLimitPerSecond, err = getInt("RATE_LIMIT_PER_SECOND", 100); err != nil {
		return Config{}, err
	}
	if cfg.WorkersPerChannel, err = getInt("WORKERS_PER_CHANNEL", 10); err != nil {
		return Config{}, err
	}
	if cfg.MaxAttempts, err = getInt("MAX_ATTEMPTS", 5); err != nil {
		return Config{}, err
	}
	durations := []struct {
		key      string
		dst      *time.Duration
		fallback time.Duration
	}{
		{"POLL_INTERVAL", &cfg.PollInterval, 200 * time.Millisecond},
		{"LOCK_TIMEOUT", &cfg.LockTimeout, 2 * time.Minute},
		{"RETRY_BASE_DELAY", &cfg.RetryBaseDelay, 2 * time.Second},
		{"RETRY_MAX_DELAY", &cfg.RetryMaxDelay, 10 * time.Minute},
		{"REAP_INTERVAL", &cfg.ReapInterval, 30 * time.Second},
	}
	for _, d := range durations {
		if *d.dst, err = getDuration(d.key, d.fallback); err != nil {
			return Config{}, err
		}
	}
	if cfg.LockTimeout <= cfg.ProviderTimeout {
		return Config{}, fmt.Errorf("config: LOCK_TIMEOUT (%s) must be longer than PROVIDER_TIMEOUT (%s)", cfg.LockTimeout, cfg.ProviderTimeout)
	}
	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

func getDuration(key string, fallback time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("config: %s must be a positive duration like 5s, got %q", key, v)
	}
	return d, nil
}
