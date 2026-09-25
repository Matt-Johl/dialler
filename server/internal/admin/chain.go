package admin

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
)

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

// Bearer refuses any request without the admin token, before any path or
// body handling, with the §4.4 envelope and nothing else. An empty token
// refuses everything.
func Bearer(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" || !BearerMatches(r, token) {
				WriteError(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Chain is the admin listener's front door, in the order of §4.7: the rate
// limit first (cheapest refusal), then the bearer guard, then the
// in-flight cap, then the API. healthz is mounted outside it by the
// caller, since it is unauthenticated.
func Chain(token string, lim *RateLimiter, st *Stats, api http.Handler) http.Handler {
	return RateLimit(lim, st)(Bearer(token)(Inflight(MaxInflight, InflightWait, st)(JSONErrors(api))))
}

// JSONErrors turns the plain-text 404 and 405 that http.ServeMux and
// http.NotFound write into the §4.4 envelope, so every admin error is
// JSON without each handler having to avoid the standard helpers.
func JSONErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&errorWriter{ResponseWriter: w}, r)
	})
}

type errorWriter struct {
	http.ResponseWriter
	swallow bool
}

func (w *errorWriter) WriteHeader(code int) {
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		w.swallow = true
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(code)
		body := ErrorBody{Error: CodeNotFound, Message: "not found"}
		if code == http.StatusMethodNotAllowed {
			body = ErrorBody{Error: CodeMethodNotAllowed, Message: "method not allowed"}
		}
		_ = json.NewEncoder(w.ResponseWriter).Encode(body)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *errorWriter) Write(b []byte) (int, error) {
	if w.swallow {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Whoami is GET /v1/admin/whoami: the cheapest authenticated call, how
// dialler-admin checks its token at start and tells "wrong token" from
// "server down".
func Whoami() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

// Healthz is GET /healthz on either listener.
func Healthz() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
}
