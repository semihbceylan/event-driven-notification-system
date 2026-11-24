package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSendSuccess(t *testing.T) {
	var got Request
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"messageId":"msg-1","status":"accepted","timestamp":"2026-11-23T12:00:00Z"}`))
	}))
	defer srv.Close()

	resp, err := NewClient(srv.URL, time.Second).Send(context.Background(), Request{
		To: "+905551234567", Channel: "sms", Content: "hi", IdempotencyKey: "n-1", CorrelationID: "c-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.MessageID != "msg-1" || resp.Status != "accepted" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if got.To != "+905551234567" || got.Channel != "sms" || got.Content != "hi" {
		t.Errorf("provider received %+v", got)
	}
	if headers.Get("Idempotency-Key") != "n-1" || headers.Get("X-Correlation-ID") != "c-1" {
		t.Errorf("missing headers: %v", headers)
	}
}

func TestSendAcceptedWithUnexpectedBodyIsStillSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, time.Second).Send(context.Background(), Request{}); err != nil {
		t.Fatalf("a 2xx must count as sent, got %v", err)
	}
}

func TestSendErrorClassification(t *testing.T) {
	tests := []struct {
		status     int
		retryAfter string
		retryable  bool
		wantAfter  time.Duration
	}{
		{status: 400, retryable: false},
		{status: 401, retryable: false},
		{status: 404, retryable: false},
		{status: 422, retryable: false},
		{status: 408, retryable: true},
		{status: 429, retryable: true, retryAfter: "30", wantAfter: 30 * time.Second},
		{status: 500, retryable: true},
		{status: 502, retryable: true},
		{status: 503, retryable: true, retryAfter: "5", wantAfter: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			_, err := NewClient(srv.URL, time.Second).Send(context.Background(), Request{})
			var pe *Error
			if !errors.As(err, &pe) {
				t.Fatalf("want *Error, got %v", err)
			}
			if pe.StatusCode != tt.status || pe.Retryable != tt.retryable || pe.RetryAfter != tt.wantAfter {
				t.Errorf("got %+v", pe)
			}
		})
	}
}

func TestSendTimeoutIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, 50*time.Millisecond).Send(context.Background(), Request{})
	pe := AsError(err)
	if !pe.Retryable || pe.StatusCode != 0 {
		t.Errorf("timeout should be retryable with no status, got %+v", pe)
	}
}

func TestSendConnectionRefusedIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing is listening any more

	_, err := NewClient(url, time.Second).Send(context.Background(), Request{})
	if pe := AsError(err); !pe.Retryable {
		t.Errorf("connection refused should be retryable, got %+v", pe)
	}
}

func TestErrorMessageIsReadable(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"error":"simulated provider failure"}`, "simulated provider failure"},
		{`{"success":false,"error":{"message":"Request limit exceeded","id":"x"}}`, "Request limit exceeded"},
		{`{"message":"quota used up"}`, "quota used up"},
		{"  upstream timed out\n", "upstream timed out"},
		{`{"unexpected":"shape"}`, `{"unexpected":"shape"}`},
		{"", ""},
	}
	for _, c := range cases {
		if got := readableMessage([]byte(c.body)); got != c.want {
			t.Errorf("readableMessage(%q) = %q, want %q", c.body, got, c.want)
		}
	}

	e := &Error{StatusCode: 503, Message: "simulated provider failure"}
	if got, want := e.Error(), "provider returned 503 Service Unavailable: simulated provider failure"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got, want := (&Error{StatusCode: 502}).Error(), "provider returned 502 Bad Gateway"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestTruncateKeepsUTF8Valid(t *testing.T) {
	got := truncate("çşğüöı", 3) // 2-byte characters: byte 3 is mid-character
	if got != "ç..." {
		t.Errorf("truncate = %q", got)
	}
}
