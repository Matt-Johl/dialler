package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// IfMatch returns the request's If-Match value with its quotes and any
// weak-validator prefix stripped, and whether one was sent (ADMIN-API.md
// §4.5). A "*" counts as not sent: it matches anything.
func IfMatch(r *http.Request) (string, bool) {
	v := strings.TrimSpace(r.Header.Get("If-Match"))
	if v == "" || v == "*" {
		return "", false
	}
	v = strings.TrimPrefix(v, "W/")
	v = strings.Trim(v, `"`)
	return v, true
}

// IfMatchTime parses an If-Match carrying an RFC 3339 time (a record's
// updated_at). ok is false when none was sent; a value that is not a
// time has already been answered 400 and the caller returns.
func IfMatchTime(w http.ResponseWriter, r *http.Request) (t *time.Time, ok, done bool) {
	v, sent := IfMatch(r)
	if !sent {
		return nil, false, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		WriteFieldError(w, http.StatusBadRequest, CodeInvalid, "If-Match must be the updated_at the record was read with", "If-Match")
		return nil, false, true
	}
	return &parsed, true, false
}

// IfMatchInt parses an If-Match carrying an integer version.
func IfMatchInt(w http.ResponseWriter, r *http.Request) (v *int64, ok, done bool) {
	s, sent := IfMatch(r)
	if !sent {
		return nil, false, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		WriteFieldError(w, http.StatusBadRequest, CodeInvalid, "If-Match must be the version the resource was read with", "If-Match")
		return nil, false, true
	}
	return &n, true, false
}

// PreconditionFailed is the §4.4 412.
func PreconditionFailed(w http.ResponseWriter) {
	WriteError(w, http.StatusPreconditionFailed, CodeVersionMismatch, "the resource changed since it was read; reload and try again")
}
