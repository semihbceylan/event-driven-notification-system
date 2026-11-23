package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/semihbceylan/event-driven-notification-system/internal/notification"
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string                    `json:"code"`
	Message string                    `json:"message"`
	Details []notification.FieldError `json:"details,omitempty"`
}

// writeError writes every error in the same shape, so clients can rely on
// error.code without parsing messages.
func writeError(w http.ResponseWriter, status int, code, message string, details []notification.FieldError) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message, Details: details}})
}

func writeValidationError(w http.ResponseWriter, details []notification.FieldError) {
	writeError(w, http.StatusUnprocessableEntity, "validation_failed", "request validation failed", details)
}
