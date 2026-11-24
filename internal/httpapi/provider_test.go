package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/semihbceylan/event-driven-notification-system/internal/provider"
)

type memSelection struct{ name string }

func (m *memSelection) Get(context.Context) (string, error)      { return m.name, nil }
func (m *memSelection) Set(_ context.Context, name string) error { m.name = name; return nil }

func TestProviderSelection(t *testing.T) {
	sel := &memSelection{}
	srv := httptest.NewServer(NewRouter(Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Notifications: &NotificationHandler{Repo: newFakeRepo()},
		Provider: &ProviderHandler{
			Targets: []provider.Target{
				{Name: "mock", URL: "http://mockprovider:8090/send"},
				{Name: "webhook.site", URL: "https://webhook.site/secret-token"},
			},
			Default:   "mock",
			Selection: sel,
		},
	}))
	t.Cleanup(srv.Close)

	resp, body := do(t, http.MethodGet, srv.URL+"/v1/provider", "", nil)
	if resp.StatusCode != http.StatusOK || body["active"] != "mock" {
		t.Fatalf("default: %d %v", resp.StatusCode, body)
	}

	resp, body = do(t, http.MethodPut, srv.URL+"/v1/provider", `{"name":"webhook.site"}`, nil)
	if resp.StatusCode != http.StatusOK || body["active"] != "webhook.site" || sel.name != "webhook.site" {
		t.Fatalf("switch: %d %v (stored %q)", resp.StatusCode, body, sel.name)
	}

	// Only configured names; a URL is never accepted.
	for _, bad := range []string{`{"name":"evil"}`, `{"name":"https://attacker.example/collect"}`, `{"url":"https://attacker.example"}`} {
		resp, _ := do(t, http.MethodPut, srv.URL+"/v1/provider", bad, nil)
		if resp.StatusCode != http.StatusUnprocessableEntity && resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 4xx", bad, resp.StatusCode)
		}
	}
	if sel.name != "webhook.site" {
		t.Errorf("a rejected request changed the selection to %q", sel.name)
	}

	// The webhook.site token must never be exposed.
	r, err := http.Get(srv.URL + "/v1/provider")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if strings.Contains(string(raw), "secret-token") {
		t.Errorf("response leaks the provider URL: %s", raw)
	}
}
