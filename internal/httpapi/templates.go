package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

// TemplateRepository is the template storage the handlers need.
type TemplateRepository interface {
	PutTemplate(ctx context.Context, name string, in notification.TemplateInput) (notification.Template, bool, error)
	GetTemplate(ctx context.Context, name string) (notification.Template, error)
	ListTemplates(ctx context.Context) ([]notification.Template, error)
	DeleteTemplate(ctx context.Context, name string) error
}

type TemplateHandler struct {
	Repo TemplateRepository
}

// Put handles PUT /v1/templates/{name}: create (201) or replace (200).
// Replacing a template does not change notifications already created from
// it, because they store the rendered text.
func (h TemplateHandler) Put(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in notification.TemplateInput
	if !decodeJSON(w, r, &in) {
		return
	}
	if errs := notification.ValidateTemplate(name, in); len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	t, created, err := h.Repo.PutTemplate(r.Context(), name, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, t)
}

func (h TemplateHandler) Get(w http.ResponseWriter, r *http.Request) {
	t, err := h.Repo.GetTemplate(r.Context(), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h TemplateHandler) List(w http.ResponseWriter, r *http.Request) {
	ts, err := h.Repo.ListTemplates(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": ts})
}

func (h TemplateHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.Repo.DeleteTemplate(r.Context(), r.PathValue("name")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h TemplateHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, notification.ErrTemplateNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "template not found", nil)
		return
	}
	logging.FromContext(r.Context()).Error("template storage error", "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
}

// resolveTemplates renders every input that names a template. Each template
// is loaded once per request, however many items use it. Field errors are
// prefixed like validation errors (e.g. "notifications[2].variables").
func resolveTemplates(ctx context.Context, repo TemplateRepository, inputs []*notification.CreateInput, prefix func(i int) string) ([]notification.FieldError, error) {
	cache := map[string]*notification.Template{} // nil value: not found
	var errs []notification.FieldError
	for i, in := range inputs {
		if in.Template == "" {
			continue
		}
		if repo == nil {
			errs = append(errs, notification.FieldError{Field: prefix(i) + "template", Message: "templates are not enabled"})
			continue
		}
		t, seen := cache[in.Template]
		if !seen {
			got, err := repo.GetTemplate(ctx, in.Template)
			switch {
			case err == nil:
				t = &got
			case errors.Is(err, notification.ErrTemplateNotFound):
				t = nil
			default:
				return nil, err
			}
			cache[in.Template] = t
		}
		if t == nil {
			errs = append(errs, notification.FieldError{Field: prefix(i) + "template", Message: fmt.Sprintf("no template named %q", in.Template)})
			continue
		}
		errs = append(errs, in.ApplyTemplate(*t, prefix(i))...)
	}
	return errs, nil
}
