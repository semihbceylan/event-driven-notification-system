// Command mockprovider imitates the external notification provider
// (the webhook.site endpoint from the assessment) so the system can run and
// be tested offline. It can also inject failures and latency, which makes
// retry and circuit-breaker behaviour easy to demonstrate.
//
// Startup settings (environment):
//
//	FAILURE_RATE  fraction of requests that fail, e.g. 0.2 (default 0)
//	LATENCY       fixed delay before each response, e.g. 50ms (default 0)
//
// Failure behaviour can be changed while running, e.g. to simulate an outage:
//
//	curl -X PUT localhost:8090/admin/config -d '{"failure_rate":1,"failure_status":503}'
//	curl -X PUT localhost:8090/admin/config -d '{"failure_rate":0}'
package main

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type sendRequest struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

type behaviour struct {
	FailureRate   float64 `json:"failure_rate"`
	FailureStatus int     `json:"failure_status"`        // default 500
	RetryAfterSec int     `json:"retry_after,omitempty"` // sent with failures if > 0
	LatencyMs     int     `json:"latency_ms"`
}

type server struct {
	mu       sync.RWMutex
	cfg      behaviour
	accepted atomic.Int64
	rejected atomic.Int64
	logger   *slog.Logger
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "mockprovider")

	failureRate, _ := strconv.ParseFloat(os.Getenv("FAILURE_RATE"), 64)
	latency, _ := time.ParseDuration(os.Getenv("LATENCY"))
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8090"
	}
	s := &server{
		cfg:    behaviour{FailureRate: failureRate, FailureStatus: http.StatusInternalServerError, LatencyMs: int(latency.Milliseconds())},
		logger: logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", s.send)
	mux.HandleFunc("GET /admin/config", s.getConfig)
	mux.HandleFunc("PUT /admin/config", s.putConfig)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	logger.Info("mock provider listening", "addr", addr, "failure_rate", failureRate, "latency", latency.String())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("mock provider stopped", "error", err)
		os.Exit(1)
	}
}

func (s *server) send(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()

	time.Sleep(time.Duration(cfg.LatencyMs) * time.Millisecond)

	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.To == "" || req.Channel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if rand.Float64() < cfg.FailureRate {
		s.rejected.Add(1)
		if cfg.RetryAfterSec > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(cfg.RetryAfterSec))
		}
		writeJSON(w, cfg.FailureStatus, map[string]string{"error": "simulated provider failure"})
		return
	}
	s.accepted.Add(1)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"messageId": uuid.NewString(),
		"status":    "accepted",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *server) getConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"config":   s.cfg,
		"accepted": s.accepted.Load(),
		"rejected": s.rejected.Load(),
	})
}

func (s *server) putConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cfg
	if err := json.NewDecoder(r.Body).Decode(&next); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if next.FailureStatus == 0 {
		next.FailureStatus = http.StatusInternalServerError
	}
	s.cfg = next
	s.logger.Info("behaviour changed", "config", next)
	writeJSON(w, http.StatusOK, next)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
