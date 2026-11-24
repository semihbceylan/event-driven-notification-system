// Package provider is the HTTP client for the external notification
// provider (webhook.site in the assessment, or the local mock).
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

type Request struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`

	// Sent as headers, not in the body, to keep the body exactly as the
	// provider contract specifies.
	IdempotencyKey string `json:"-"`
	CorrelationID  string `json:"-"`
}

type Response struct {
	MessageID string `json:"messageId"`
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// Error is a failed send. Retryable tells the worker whether trying again
// later can help.
type Error struct {
	StatusCode int           // 0 if no response was received
	Retryable  bool          //
	RetryAfter time.Duration // from the Retry-After header, if present
	Message    string
}

func (e *Error) Error() string {
	if e.StatusCode == 0 {
		return e.Message
	}
	text := fmt.Sprintf("provider returned %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	if e.Message == "" {
		return text
	}
	return text + ": " + e.Message
}

type Client struct {
	url  string
	http *http.Client
}

func NewClient(url string, timeout time.Duration) *Client {
	return &Client{url: url, http: &http.Client{
		Timeout: timeout,
		// A client span per provider call, and a traceparent header so a
		// provider that supports tracing can join the trace.
		Transport: otelhttp.NewTransport(http.DefaultTransport),
	}}
}

// Send delivers one message. Every failure is returned as *Error.
func (c *Client) Send(ctx context.Context, req Request) (Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, &Error{Message: "encode request: " + err.Error()}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Response{}, &Error{Message: "build request: " + err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.IdempotencyKey != "" {
		httpReq.Header.Set("Idempotency-Key", req.IdempotencyKey)
	}
	if req.CorrelationID != "" {
		httpReq.Header.Set("X-Correlation-ID", req.CorrelationID)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Timeouts, refused connections, DNS failures: the provider may be
		// briefly unavailable, so these are worth retrying.
		return Response{}, &Error{Retryable: true, Message: "request failed: " + err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var out Response
		// The message was accepted even if the body is not what we expect,
		// so a decode problem must not turn into a retry (that would send
		// the message twice).
		_ = json.Unmarshal(respBody, &out)
		return out, nil
	}

	return Response{}, &Error{
		StatusCode: resp.StatusCode,
		Retryable:  isRetryable(resp.StatusCode),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
		Message:    readableMessage(respBody),
	}
}

// readableMessage turns an error response body into text a person can read
// at a glance. Providers usually answer with JSON such as
// {"error":"simulated provider failure"} or
// {"success":false,"error":{"message":"Request limit exceeded"}}; showing
// that raw puts the useful part at the end, where the dashboard cuts it
// off. Anything that is not such JSON is kept as it is.
func readableMessage(body []byte) string {
	var v map[string]any
	if json.Unmarshal(body, &v) == nil {
		for _, key := range []string{"error", "message", "detail", "error_description"} {
			switch x := v[key].(type) {
			case string:
				if x != "" {
					return truncate(x, 500)
				}
			case map[string]any:
				for _, inner := range []string{"message", "detail", "code"} {
					if m, ok := x[inner].(string); ok && m != "" {
						return truncate(m, 500)
					}
				}
			}
		}
	}
	return truncate(strings.TrimSpace(string(body)), 500)
}

// isRetryable classifies HTTP status codes:
//   - 408 Request Timeout, 429 Too Many Requests and 5xx are temporary.
//   - Every other 4xx means the request itself is wrong (bad number,
//     rejected content). Sending it again would fail the same way.
func isRetryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// parseRetryAfter supports both forms of the header: seconds ("120") and an
// HTTP date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// truncate cuts s to at most n bytes without splitting a UTF-8 character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// AsError unwraps err into a *Error, wrapping unexpected errors as
// retryable so nothing is silently dropped.
func AsError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Retryable: true, Message: err.Error()}
}
