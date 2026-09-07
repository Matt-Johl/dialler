package enroll

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Hooks lets the server react to enrolment changes (e.g. provision the
// SIP registry). Nil funcs are ignored.
type Hooks struct {
	OnIssue  func(deviceID, user string)
	OnRevoke func(deviceID string)
}

// NewAdminHandler serves the admin enrolment API, guarded by a static
// bearer token:
//
//	POST   /v1/admin/devices        {"device_id","user"[,"token"]} → {"device_id","user","token"}
//	GET    /v1/admin/devices        → [Device]
//	DELETE /v1/admin/devices/{id}   → 204
func NewAdminHandler(store *Store, adminToken string, hooks Hooks) http.Handler {
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || !BearerMatches(r, adminToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("POST /v1/admin/devices", guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			DeviceID string `json:"device_id"`
			User     string `json:"user"`
			// Optional fixed token (dev fixtures); random when absent.
			Token string `json:"token,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		tok, err := store.IssueToken(in.DeviceID, in.User, in.Token)
		if errors.Is(err, ErrInvalid) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "enrolment store: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if hooks.OnIssue != nil {
			hooks.OnIssue(in.DeviceID, in.User)
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"device_id": in.DeviceID, "user": in.User, "token": tok,
		})
	}))

	mux.HandleFunc("GET /v1/admin/devices", guard(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, store.Devices())
	}))

	mux.HandleFunc("DELETE /v1/admin/devices/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ok, err := store.Revoke(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if hooks.OnRevoke != nil {
			hooks.OnRevoke(id)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// BearerMatches reports whether the request carries "Authorization: Bearer
// <want>", compared in constant time.
func BearerMatches(r *http.Request, want string) bool {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if !strings.HasPrefix(h, p) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, p)), []byte(want)) == 1
}

// DeviceAuth is HTTP middleware that authenticates a device by
// "X-Device-ID" + "Authorization: Bearer <token>" against the store.
func (s *Store) DeviceAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Device-ID")
		h := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(h, "Bearer ")
		if id == "" || tok == h {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ok, _ := s.Authenticate(r.Context(), id, tok)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
