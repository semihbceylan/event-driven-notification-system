package httpapi

import (
	"net/http"

	"github.com/semihbceylan/event-driven-notification-system/internal/logging"
	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
	"github.com/semihbceylan/event-driven-notification-system/internal/provider"
)

// ProviderHandler shows and switches the active delivery provider.
//
// Only providers configured on the server (PROVIDERS) can be selected, and
// only by name. Accepting a URL would let any caller redirect every
// notification, recipients and content included, to a server of their
// choosing. In production this endpoint would also need admin auth.
type ProviderHandler struct {
	Targets   []provider.Target
	Default   string
	Selection provider.Selection
}

type providerResponse struct {
	Active    string          `json:"active"`
	Providers []provider.Info `json:"providers"`
}

// Get handles GET /v1/provider.
func (h ProviderHandler) Get(w http.ResponseWriter, r *http.Request) {
	active, err := provider.Active(r.Context(), h.Selection, h.Targets, h.Default)
	if err != nil {
		logging.FromContext(r.Context()).Error("read active provider", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	writeJSON(w, http.StatusOK, providerResponse{Active: active, Providers: provider.Infos(h.Targets)})
}

// Put handles PUT /v1/provider {"name": "..."}. Workers pick up the change
// within about a second and reset their circuit breakers.
func (h ProviderHandler) Put(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	known := false
	names := ""
	for i, t := range h.Targets {
		if t.Name == body.Name {
			known = true
		}
		if i > 0 {
			names += ", "
		}
		names += t.Name
	}
	if !known {
		writeValidationError(w, []notification.FieldError{{Field: "name", Message: "must be one of the configured providers: " + names}})
		return
	}
	if err := h.Selection.Set(r.Context(), body.Name); err != nil {
		logging.FromContext(r.Context()).Error("set active provider", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "internal server error", nil)
		return
	}
	logging.FromContext(r.Context()).Info("provider selected", "provider", body.Name)
	writeJSON(w, http.StatusOK, providerResponse{Active: body.Name, Providers: provider.Infos(h.Targets)})
}
