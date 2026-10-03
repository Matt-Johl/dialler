package licence

import (
	"errors"
	"net/http"
	"strings"

	"dialler/server/internal/admin"
)

// bodyLimit bounds a licence PUT: the token's own cap plus the JSON around
// it. ADMIN-API.md §4.2 / §5.13.
const bodyLimit = MaxLen + 256

// NewHandler is the admin API's licence resource (SPEC §4.9):
//
//	GET  /v1/admin/licence                 → Summary
//	PUT  /v1/admin/licence  {"licence"}    → Summary   (POST accepted too)
//
// A PUT verifies the token, binds it to this install, refuses one that is
// not a licence, is for another server or has expired (400 invalid, with
// the reason the operator needs), and stores it. The licence is the one
// server-wide setting the API persists (ADMIN-API.md rules 2 and 3 carry
// the exception): it is data, kept at <data-dir>/licence, and a change
// touches exactly the devices whose seat flips, never a call.
func NewHandler(m *Manager, adminToken string, seatsUsed func() int) http.Handler {
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || !admin.BearerMatches(r, adminToken) {
				admin.WriteError(w, http.StatusUnauthorized, admin.CodeUnauthorized, "unauthorized")
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/admin/licence", guard(func(w http.ResponseWriter, r *http.Request) {
		admin.WriteJSON(w, http.StatusOK, m.Summary(seatsUsed()))
	}))
	install := guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Licence string `json:"licence"`
		}
		if !admin.Decode(w, r, bodyLimit, &in) {
			return
		}
		if strings.TrimSpace(in.Licence) == "" {
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "licence is required", "licence")
			return
		}
		_, err := m.Install(in.Licence)
		switch {
		case err == nil:
			admin.WriteJSON(w, http.StatusOK, m.Summary(seatsUsed()))
		case errors.Is(err, ErrFormat), errors.Is(err, ErrSignature), errors.Is(err, ErrPayload),
			errors.Is(err, ErrWrongInstall), errors.Is(err, ErrExpired):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, reason(err), "licence")
		default:
			admin.StoreError(w, err)
		}
	})
	mux.HandleFunc("PUT /v1/admin/licence", install)
	mux.HandleFunc("POST /v1/admin/licence", install)
	return mux
}

// reason is the refusal in the operator's terms: the package prefix and
// the sentinel's own words are dropped in favour of the detail each error
// carries, which already names the id, the date or the fault.
func reason(err error) string {
	msg := err.Error()
	for _, p := range []string{"licence: not a licence token: ", "licence: payload is not valid: ", "licence: issued for another install id: ", "licence: "} {
		if strings.HasPrefix(msg, p) {
			msg = strings.TrimPrefix(msg, p)
			break
		}
	}
	switch {
	case errors.Is(err, ErrFormat):
		return "not a licence token: " + msg
	case errors.Is(err, ErrSignature):
		return "signature does not verify: the token was altered or was not issued by the vendor"
	case errors.Is(err, ErrPayload):
		return "payload is not valid: " + msg
	case errors.Is(err, ErrExpired):
		return msg // "expired on DATE"
	}
	return msg
}
