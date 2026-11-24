package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestLogLevel(t *testing.T) {
	cases := []struct {
		route  string
		status int
		want   slog.Level
	}{
		{"POST /v1/notifications", 201, slog.LevelInfo},
		{"GET /healthz", 200, slog.LevelDebug},
		{"GET /v1/metrics", 200, slog.LevelDebug},
		{"GET /v1/metrics", 422, slog.LevelWarn}, // a failing poll is still visible
		{"GET /readyz", 503, slog.LevelError},
		{"POST /v1/notifications", 409, slog.LevelWarn},
		{"", 404, slog.LevelWarn},
	}
	for _, c := range cases {
		if got := requestLogLevel(c.route, c.status); got != c.want {
			t.Errorf("requestLogLevel(%q, %d) = %v, want %v", c.route, c.status, got, c.want)
		}
	}
}

// At the default info level, health checks leave no log line but real
// requests do.
func TestPollingRequestsAreNotLoggedAtInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /v1/notifications", ok)
	h := RequestContext(logger, nil)(mux)

	for _, path := range []string{"/healthz", "/v1/notifications"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	var routes []string
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var entry struct{ Route string }
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("bad log line %q: %v", line, err)
		}
		routes = append(routes, entry.Route)
	}
	if len(routes) != 1 || routes[0] != "GET /v1/notifications" {
		t.Fatalf("logged routes = %v, want only GET /v1/notifications", routes)
	}
}
