// Command mockprovider imitates the external notification provider
// (the webhook.site endpoint from the assessment) so the system can run and
// be tested offline. It can also inject failures and latency, which makes
// retry and circuit-breaker behaviour easy to demonstrate.
//
// Environment:
//
//	FAILURE_RATE  fraction of requests answered with 500, e.g. 0.2 (default 0)
//	LATENCY       fixed delay before each response, e.g. 50ms (default 0)
package main

import (
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
)

type sendRequest struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "mockprovider")

	failureRate, _ := strconv.ParseFloat(os.Getenv("FAILURE_RATE"), 64)
	latency, _ := time.ParseDuration(os.Getenv("LATENCY"))
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8090"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(latency)

		var req sendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.To == "" || req.Channel == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		if rand.Float64() < failureRate {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "simulated provider failure"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]string{
			"messageId": uuid.NewString(),
			"status":    "accepted",
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		})
	})
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
