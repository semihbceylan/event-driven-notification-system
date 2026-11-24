package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Target is a provider the operator configured, e.g. mock or webhook.site.
type Target struct {
	Name string
	URL  string
}

// Info describes a target to API clients. It deliberately leaves out the
// URL: a webhook.site URL is effectively a secret (anyone holding it can
// read every message sent to it).
type Info struct {
	Name string `json:"name"`
	Host string `json:"host"`
	// Mock is true for the built-in mock provider, whose failures can be
	// controlled (the dashboard's "Simulate outage").
	Mock bool `json:"mock"`
}

// ParseTargets reads "name=url,name=url". Entries with an empty URL are
// skipped, so docker compose can list optional providers whose URL may be
// unset.
func ParseTargets(spec string) ([]Target, error) {
	var out []Target
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, raw, ok := strings.Cut(part, "=")
		name, raw = strings.TrimSpace(name), strings.TrimSpace(raw)
		if !ok || name == "" {
			return nil, fmt.Errorf("provider %q: want name=url", part)
		}
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("provider %q: %q is not an http(s) URL", name, raw)
		}
		if seen[name] {
			return nil, fmt.Errorf("provider %q is listed twice", name)
		}
		seen[name] = true
		out = append(out, Target{Name: name, URL: raw})
	}
	if len(out) == 0 {
		return nil, errors.New("no providers configured")
	}
	return out, nil
}

// Infos returns what API clients may see about each target.
func Infos(targets []Target) []Info {
	out := make([]Info, len(targets))
	for i, t := range targets {
		host := t.URL
		if u, err := url.Parse(t.URL); err == nil {
			host = u.Host
		}
		out[i] = Info{Name: t.Name, Host: host, Mock: t.Name == "mock"}
	}
	return out
}

// Selection stores which provider is active, shared by every process.
type Selection interface {
	Get(ctx context.Context) (string, error) // "" if never set
	Set(ctx context.Context, name string) error
}

// RedisSelection keeps the active provider's name in Redis.
type RedisSelection struct{ RDB *redis.Client }

const selectionKey = "provider:active"

func (s RedisSelection) Get(ctx context.Context) (string, error) {
	v, err := s.RDB.Get(ctx, selectionKey).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return v, err
}

func (s RedisSelection) Set(ctx context.Context, name string) error {
	return s.RDB.Set(ctx, selectionKey, name, 0).Err()
}

// Active resolves the selection to a configured target name: the stored
// one if it is still configured, otherwise the default. A stale name (say a
// provider removed from the config) can never send traffic anywhere.
func Active(ctx context.Context, sel Selection, targets []Target, def string) (string, error) {
	name, err := sel.Get(ctx)
	if err != nil {
		return "", err
	}
	for _, t := range targets {
		if t.Name == name {
			return name, nil
		}
	}
	return def, nil
}

// Switch sends through whichever configured provider is currently active.
// The worker uses it as its Sender. Refresh (or Run) picks up changes made
// through the API.
type Switch struct {
	targets []Target
	clients map[string]*Client
	def     string
	sel     Selection
	logger  *slog.Logger

	mu       sync.RWMutex
	active   string
	onChange func(from, to string)
}

// NewSwitch starts on def, which must be one of targets.
func NewSwitch(targets []Target, def string, timeout time.Duration, sel Selection, logger *slog.Logger) (*Switch, error) {
	s := &Switch{targets: targets, clients: map[string]*Client{}, def: def, sel: sel, logger: logger, active: def}
	for _, t := range targets {
		s.clients[t.Name] = NewClient(t.URL, timeout)
	}
	if _, ok := s.clients[def]; !ok {
		return nil, fmt.Errorf("default provider %q is not configured", def)
	}
	return s, nil
}

// OnChange registers a callback for when the active provider changes.
func (s *Switch) OnChange(fn func(from, to string)) { s.onChange = fn }

func (s *Switch) ActiveName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}

func (s *Switch) Send(ctx context.Context, req Request) (Response, error) {
	s.mu.RLock()
	c := s.clients[s.active]
	s.mu.RUnlock()
	return c.Send(ctx, req)
}

// Refresh reads the shared selection and switches if it changed.
func (s *Switch) Refresh(ctx context.Context) error {
	name, err := Active(ctx, s.sel, s.targets, s.def)
	if err != nil {
		return err
	}
	s.mu.Lock()
	from := s.active
	s.active = name
	s.mu.Unlock()
	if from != name {
		s.logger.Info("provider switched", "from", from, "to", name)
		if s.onChange != nil {
			s.onChange(from, name)
		}
	}
	return nil
}

// Run refreshes every interval until ctx is cancelled. If Redis is
// unreachable the current provider stays in use.
func (s *Switch) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
			s.logger.Warn("could not read the active provider; keeping the current one", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
