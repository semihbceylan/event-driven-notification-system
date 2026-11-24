package provider

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type memSelection struct{ name string }

func (m *memSelection) Get(context.Context) (string, error)      { return m.name, nil }
func (m *memSelection) Set(_ context.Context, name string) error { m.name = name; return nil }

func TestParseTargets(t *testing.T) {
	got, err := ParseTargets(" mock=http://mockprovider:8090/send , webhook.site= ,hook=https://webhook.site/abc ")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "mock" || got[1].Name != "hook" {
		t.Errorf("got %+v; the entry with an empty URL should be skipped", got)
	}

	for _, bad := range []string{"", "mock", "=http://x", "a=ftp://x", "a=http://x,a=http://y", "a=not a url"} {
		if _, err := ParseTargets(bad); err == nil {
			t.Errorf("ParseTargets(%q) should fail", bad)
		}
	}
}

func TestInfosHideURLs(t *testing.T) {
	infos := Infos([]Target{{Name: "mock", URL: "http://mockprovider:8090/send"}, {Name: "webhook.site", URL: "https://webhook.site/secret-token"}})
	if infos[0] != (Info{Name: "mock", Host: "mockprovider:8090", Mock: true}) {
		t.Errorf("mock info = %+v", infos[0])
	}
	if infos[1].Host != "webhook.site" || infos[1].Mock {
		t.Errorf("webhook info = %+v; the token in the path must not be exposed", infos[1])
	}
}

func TestSwitchRoutesToActiveProvider(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	srv := func(hits *atomic.Int32) *httptest.Server {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(s.Close)
		return s
	}
	a, b := srv(&hitsA), srv(&hitsB)

	sel := &memSelection{}
	sw, err := NewSwitch([]Target{{"a", a.URL}, {"b", b.URL}}, "a", time.Second, sel, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	var changes []string
	sw.OnChange(func(from, to string) { changes = append(changes, from+"->"+to) })
	ctx := context.Background()

	if _, err := sw.Send(ctx, Request{}); err != nil || hitsA.Load() != 1 {
		t.Fatalf("default provider not used: err=%v hitsA=%d", err, hitsA.Load())
	}

	_ = sel.Set(ctx, "b")
	if err := sw.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Send(ctx, Request{}); err != nil || hitsB.Load() != 1 {
		t.Fatalf("after switching to b: err=%v hitsB=%d", err, hitsB.Load())
	}

	// A name that is not configured falls back to the default.
	_ = sel.Set(ctx, "removed-provider")
	_ = sw.Refresh(ctx)
	if sw.ActiveName() != "a" {
		t.Errorf("active = %s, want default a", sw.ActiveName())
	}

	if len(changes) != 2 || changes[0] != "a->b" || changes[1] != "b->a" {
		t.Errorf("changes = %v", changes)
	}
}

func TestNewSwitchRejectsUnknownDefault(t *testing.T) {
	if _, err := NewSwitch([]Target{{"mock", "http://x"}}, "other", time.Second, &memSelection{}, slog.Default()); err == nil {
		t.Error("expected an error for an unconfigured default")
	}
}
