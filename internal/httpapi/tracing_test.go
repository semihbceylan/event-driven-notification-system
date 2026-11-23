package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

// useRecorder installs an in-memory tracer for the duration of a test.
func useRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec
}

func TestCreateStoresTraceContext(t *testing.T) {
	rec := useRecorder(t)
	repo := newFakeRepo()
	srv := httptest.NewServer(NewRouter(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notifications: &NotificationHandler{Repo: repo, MaxAttempts: 5},
	}))
	t.Cleanup(srv.Close)

	// The caller is itself traced: our span must join its trace.
	const callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/notifications", strings.NewReader(smsBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("traceparent", "00-"+callerTrace+"-00f067aa0ba902b7-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}

	var stored notification.Notification
	for _, n := range repo.byID {
		stored = n
	}
	if stored.TraceParent == nil || !strings.Contains(*stored.TraceParent, callerTrace) {
		t.Fatalf("stored traceparent = %v, want trace %s", stored.TraceParent, callerTrace)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Name() != "POST /v1/notifications" || spans[0].SpanContext().TraceID().String() != callerTrace {
		t.Errorf("span %q in trace %s", spans[0].Name(), spans[0].SpanContext().TraceID())
	}
	// The worker's span will be a child of this one: the stored value
	// must point at it.
	if !strings.Contains(*stored.TraceParent, spans[0].SpanContext().SpanID().String()) {
		t.Errorf("stored traceparent %s does not reference the request span", *stored.TraceParent)
	}
}

func TestHealthChecksAreNotTraced(t *testing.T) {
	rec := useRecorder(t)
	srv := httptest.NewServer(NewRouter(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notifications: &NotificationHandler{Repo: newFakeRepo()},
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n := len(rec.Ended()); n != 0 {
		t.Errorf("health check produced %d spans", n)
	}
}
