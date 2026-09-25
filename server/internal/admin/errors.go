// Package admin is the admin API's front door: the bearer guard, the bounds
// that keep admin traffic from degrading a call, and the JSON error
// envelope every admin route answers with (protocol/ADMIN-API.md §4).
//
// It holds no routes of its own beyond whoami and healthz; the resource
// handlers live with their stores (enroll, directory, …) and are mounted
// behind Chain by the server's main.
package admin

import (
	"encoding/json"
	"net/http"
)

// Error codes, stable across v1 (ADMIN-API.md §4.4). Clients switch on
// these, never on the message.
const (
	CodeBadJSON              = "bad_json"
	CodeUnknownField         = "unknown_field"
	CodeInvalid              = "invalid"
	CodeMissing              = "missing"
	CodeUnauthorized         = "unauthorized"
	CodeNotFound             = "not_found"
	CodeMethodNotAllowed     = "method_not_allowed"
	CodeDeviceExists         = "device_exists"
	CodeUserTaken            = "user_taken"
	CodeImmutable            = "immutable"
	CodeVersionMismatch      = "version_mismatch"
	CodeTooLarge             = "too_large"
	CodeUnsupportedMediaType = "unsupported_media_type"
	CodeRateLimited          = "rate_limited"
	CodeStore                = "store"
	CodeUnavailable          = "unavailable"
	CodeBusy                 = "busy"
)

// ErrorBody is the envelope of every non-2xx admin answer.
type ErrorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	// Field names the JSON field at fault on a 400, dotted for nesting
	// ("contacts[3].uri"); omitted otherwise.
	Field string `json:"field,omitempty"`
}

// WriteError answers with the envelope. message is one sentence for a
// human and may change; code may not.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: message})
}

// WriteFieldError is WriteError naming the field at fault.
func WriteFieldError(w http.ResponseWriter, status int, code, message, field string) {
	WriteJSON(w, status, ErrorBody{Error: code, Message: message, Field: field})
}

// WriteJSON writes v as the response body with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
