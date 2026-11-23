package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/semihbceylan/event-driven-notification-system/api"
)

// TestOpenAPIMatchesRouter fails when a route is added without documenting
// it, or when the spec describes an endpoint that does not exist. Docs that
// silently drift from the code are worse than no docs.
func TestOpenAPIMatchesRouter(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.OpenAPI, &spec); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	documented := map[string]bool{}
	for path, ops := range spec.Paths {
		for method := range ops {
			switch method {
			case "get", "post", "put", "patch", "delete":
				documented[strings.ToUpper(method)+" "+path] = true
			}
		}
	}

	all := Deps{
		Notifications: &NotificationHandler{},
		Metrics:       &MetricsHandler{},
		Prometheus:    http.NotFoundHandler(),
		WS:            &WSHandler{},
		Templates:     &TemplateHandler{},
	}
	served := map[string]bool{}
	for _, r := range routes(all) {
		if !r.documented {
			continue
		}
		served[r.pattern] = true
		if !documented[r.pattern] {
			t.Errorf("route %q is not in api/openapi.yaml", r.pattern)
		}
	}
	for op := range documented {
		if !served[op] {
			t.Errorf("api/openapi.yaml documents %q, but the router does not serve it", op)
		}
	}
}

func TestDocsAreServed(t *testing.T) {
	srv, _ := newTestServer(t)
	for path, wantType := range map[string]string{"/openapi.yaml": "application/yaml", "/docs": "text/html"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), wantType) {
			t.Errorf("%s: status %d, content-type %q", path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
}

// TestOpenAPIRefsResolve checks that every "$ref" points at something that
// exists, the most common way a hand-written spec breaks.
func TestOpenAPIRefsResolve(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}

	var refs []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				if s, ok := child.(string); ok && k == "$ref" {
					refs = append(refs, s)
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(doc)

	if len(refs) == 0 {
		t.Fatal("found no $refs; the walk is broken")
	}
	for _, ref := range refs {
		parts := strings.Split(strings.TrimPrefix(ref, "#/"), "/")
		var node any = doc
		for _, p := range parts {
			m, ok := node.(map[string]any)
			if !ok {
				node = nil
				break
			}
			node = m[p]
		}
		if !strings.HasPrefix(ref, "#/") || node == nil {
			t.Errorf("unresolved $ref %q", ref)
		}
	}
}
