package notification

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Template is reusable content with {{variable}} placeholders.
//
// We use a small placeholder syntax rather than Go's text/template on
// purpose: templates are written by API clients, and text/template supports
// loops, function calls and arbitrarily expensive logic. Plain substitution
// is all a notification needs, and it cannot be abused.
type Template struct {
	Name      string    `json:"name"`
	Channel   Channel   `json:"channel"`
	Content   string    `json:"content"`
	Variables []string  `json:"variables"` // placeholders found in Content, in order of first use
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var (
	// {{ name }}: spaces inside the braces are allowed.
	placeholderRE  = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	templateNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

var ErrTemplateNotFound = errors.New("template not found")

// TemplateInput is the body of PUT /v1/templates/{name}.
type TemplateInput struct {
	Channel Channel `json:"channel"`
	Content string  `json:"content"`
}

// ValidateTemplate checks a template before it is saved, so problems show
// up when the template is written, not when someone first uses it.
func ValidateTemplate(name string, in TemplateInput) []FieldError {
	var errs []FieldError
	if !templateNameRE.MatchString(name) {
		errs = append(errs, FieldError{Field: "name", Message: "must be 1-64 characters: lowercase letters, digits, '-' or '_', starting with a letter or digit"})
	}
	if !in.Channel.Valid() {
		errs = append(errs, FieldError{Field: "channel", Message: "must be one of: sms, email, push"})
	}
	switch {
	case strings.TrimSpace(in.Content) == "":
		errs = append(errs, FieldError{Field: "content", Message: "is required"})
	case malformedPlaceholder(in.Content):
		errs = append(errs, FieldError{Field: "content", Message: "has a malformed placeholder; use {{name}} with letters, digits and underscores"})
	default:
		if limit, ok := contentLimit(in.Channel); ok && utf8.RuneCountInString(in.Content) > limit {
			errs = append(errs, FieldError{Field: "content", Message: fmt.Sprintf("is longer than the %s limit of %d characters", in.Channel, limit)})
		}
	}
	return errs
}

// Placeholders returns the variable names used in content, in order of
// first appearance, without duplicates.
func Placeholders(content string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range placeholderRE.FindAllStringSubmatch(content, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}

// Render substitutes variables into content. Every placeholder must have a
// value; the names of any that are missing are returned, sorted. Extra
// variables are ignored, so a client can send one set of variables to
// several templates.
func Render(content string, vars map[string]string) (string, []string) {
	missing := map[string]bool{}
	out := placeholderRE.ReplaceAllStringFunc(content, func(m string) string {
		name := placeholderRE.FindStringSubmatch(m)[1]
		v, ok := vars[name]
		if !ok {
			missing[name] = true
			return m
		}
		return v
	})
	if len(missing) == 0 {
		return out, nil
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", names
}

// malformedPlaceholder reports braces that are not part of a valid
// placeholder, e.g. "{{ first name }}" or "{{name}".
func malformedPlaceholder(content string) bool {
	rest := placeholderRE.ReplaceAllString(content, "")
	return strings.Contains(rest, "{{") || strings.Contains(rest, "}}")
}

// ApplyTemplate renders t into in.Content. It is called after Normalize and
// before Validate, so the rendered text still goes through the channel's
// normal checks (length limits etc.).
func (in *CreateInput) ApplyTemplate(t Template, prefix string) []FieldError {
	if in.Content != "" {
		return []FieldError{{Field: prefix + "content", Message: "must be empty when template is set"}}
	}
	if in.Channel != t.Channel {
		return []FieldError{{Field: prefix + "template", Message: fmt.Sprintf("is a %s template and cannot be used for %s", t.Channel, in.Channel)}}
	}
	rendered, missing := Render(t.Content, in.Variables)
	if len(missing) > 0 {
		return []FieldError{{Field: prefix + "variables", Message: "missing values for: " + strings.Join(missing, ", ")}}
	}
	in.Content = rendered
	return nil
}

// --- storage ---

// PutTemplate creates or replaces a template. created is true if it is new.
func (s *Store) PutTemplate(ctx context.Context, name string, in TemplateInput) (t Template, created bool, err error) {
	// xmax = 0 is Postgres's way of telling an inserted row from an updated
	// one in an upsert.
	err = s.pool.QueryRow(ctx, `
		INSERT INTO templates (name, channel, content) VALUES ($1, $2, $3)
		ON CONFLICT (name) DO UPDATE SET channel = EXCLUDED.channel, content = EXCLUDED.content, updated_at = now()
		RETURNING name, channel, content, created_at, updated_at, (xmax = 0)`,
		name, string(in.Channel), in.Content,
	).Scan(&t.Name, &t.Channel, &t.Content, &t.CreatedAt, &t.UpdatedAt, &created)
	if err != nil {
		return Template{}, false, fmt.Errorf("put template: %w", err)
	}
	t.Variables = Placeholders(t.Content)
	return t, created, nil
}

func (s *Store) GetTemplate(ctx context.Context, name string) (Template, error) {
	var t Template
	err := s.pool.QueryRow(ctx,
		`SELECT name, channel, content, created_at, updated_at FROM templates WHERE name = $1`, name,
	).Scan(&t.Name, &t.Channel, &t.Content, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrTemplateNotFound
	}
	if err != nil {
		return Template{}, fmt.Errorf("get template: %w", err)
	}
	t.Variables = Placeholders(t.Content)
	return t, nil
}

func (s *Store) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, channel, content, created_at, updated_at FROM templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()
	out := []Template{}
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.Name, &t.Channel, &t.Content, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		t.Variables = Placeholders(t.Content)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTemplate(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM templates WHERE name = $1`, name)
	if err != nil {
		return fmt.Errorf("delete template: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTemplateNotFound
	}
	return nil
}
