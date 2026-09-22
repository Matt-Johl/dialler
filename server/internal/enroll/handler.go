package enroll

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hooks lets the server react to enrolment changes (e.g. provision the
// SIP registry). Nil funcs are ignored.
type Hooks struct {
	// OnIssue: a device was created or given a credential by the admin.
	OnIssue func(deviceID, user string)
	// OnRevoke: the admin revoked a device.
	OnRevoke func(deviceID string)
	// OnClaim: a device claimed its enrolment code and holds a NEW
	// credential; whatever was connected with the old one must go.
	OnClaim func(deviceID, user string)
	// OnConfig: the admin changed a device's settings; push them to it.
	OnConfig func(deviceID string, cfg DeviceConfig)
	// OnPBXLine: the admin set or removed a device's PBX line (nil =
	// removed). The registrar re-registers that one line and no other.
	OnPBXLine func(deviceID string, line *PBXCredential)
}

// Link is what a phone needs to find the server, folded into the QR URL
// an enrolment code is shown as (SPEC §4.8):
//
//	dialler://enrol?h=<host>&p=<https port>&c=<code>&f=<cert sha256, base64url>
type Link struct {
	Host       string
	HTTPSPort  int
	CertSHA256 string
}

// URL renders the QR link for code.
func (l Link) URL(code string) string {
	q := url.Values{}
	q.Set("h", l.Host)
	q.Set("p", strconv.Itoa(l.HTTPSPort))
	q.Set("c", code)
	if l.CertSHA256 != "" {
		q.Set("f", l.CertSHA256)
	}
	return "dialler://enrol?" + q.Encode()
}

type codeResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
	URL       string    `json:"url"`
}

// NewAdminHandler serves the admin enrolment API, guarded by a static
// bearer token:
//
//	POST   /v1/admin/devices                  {"user"[,"device_id","label","token"]}
//	                                          → 201 {"device_id","user","label","code","expires_at","url"[,"token"]}
//	GET    /v1/admin/devices                  → [Device]
//	DELETE /v1/admin/devices/{id}             → 204 (revoke)
//	POST   /v1/admin/devices/{id}/enrol-code  → {"code","expires_at","url"}
//	GET    /v1/admin/devices/{id}/config      → {"version","ssids"} (404 until set)
//	PUT    /v1/admin/devices/{id}/config      {"ssids":[…]} → {"version","ssids"}   (POST accepted too)
//	GET    /v1/admin/devices/{id}/pbx-line    → {"dn","digest_user","configured"}  (404 until set)
//	PUT    /v1/admin/devices/{id}/pbx-line    {"digest_user","secret"[,"dn"]} → the same view (POST too)
//	DELETE /v1/admin/devices/{id}/pbx-line    → 204
//
// The PBX line's secret is write-only: it goes in, and nothing — no read, no
// device, no log — gets it back out (SPEC §6 item 3c).
//
// Adding a device mints its enrolment code; a "token" in the request (the
// harness's fixed fixtures) also issues that credential at once.
func NewAdminHandler(store *Store, adminToken string, link Link, hooks Hooks) http.Handler {
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
			Label    string `json:"label"`
			// Optional fixed token (dev fixtures); no credential is issued
			// when absent — the device claims its code for one.
			Token string `json:"token,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		id, err := store.Create(in.DeviceID, in.User, in.Label)
		if errors.Is(err, ErrInvalid) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "enrolment store: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out := map[string]any{"device_id": id, "user": in.User, "label": in.Label}
		if in.Token != "" {
			tok, err := store.IssueToken(id, in.User, in.Token)
			if errors.Is(err, ErrInvalid) {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, "enrolment store: "+err.Error(), http.StatusInternalServerError)
				return
			}
			out["token"] = tok
		}
		code, expires, err := store.MintCode(id)
		if err != nil {
			http.Error(w, "enrolment store: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out["code"], out["expires_at"], out["url"] = code, expires, link.URL(code)
		if hooks.OnIssue != nil {
			hooks.OnIssue(id, in.User)
		}
		writeJSON(w, http.StatusCreated, out)
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

	mux.HandleFunc("POST /v1/admin/devices/{id}/enrol-code", guard(func(w http.ResponseWriter, r *http.Request) {
		code, expires, err := store.MintCode(r.PathValue("id"))
		if errors.Is(err, ErrInvalid) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, codeResponse{Code: code, ExpiresAt: expires, URL: link.URL(code)})
	}))

	mux.HandleFunc("GET /v1/admin/devices/{id}/config", guard(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := store.UserFor(id); !ok {
			http.NotFound(w, r)
			return
		}
		cfg := store.Config(id)
		if cfg == nil {
			http.Error(w, "no settings set for this device", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, cfg)
	}))
	setConfig := guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SSIDs []string `json:"ssids"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil || in.SSIDs == nil {
			http.Error(w, "bad json: want {\"ssids\":[…]}", http.StatusBadRequest)
			return
		}
		id := r.PathValue("id")
		cfg, err := store.SetConfig(id, in.SSIDs)
		if errors.Is(err, ErrInvalid) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if hooks.OnConfig != nil {
			hooks.OnConfig(id, cfg)
		}
		writeJSON(w, http.StatusOK, cfg)
	})
	mux.HandleFunc("PUT /v1/admin/devices/{id}/config", setConfig)
	// busybox wget (the harness's in-network helper) has no PUT.
	mux.HandleFunc("POST /v1/admin/devices/{id}/config", setConfig)

	mux.HandleFunc("GET /v1/admin/devices/{id}/pbx-line", guard(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := store.UserFor(id); !ok {
			http.NotFound(w, r)
			return
		}
		line, ok := store.PBXLine(id)
		if !ok {
			http.Error(w, "no PBX line set for this device", http.StatusNotFound)
			return
		}
		// Deliberately the view type: there is no query parameter, no
		// header and no debug mode that returns the secret.
		writeJSON(w, http.StatusOK, line)
	}))
	setLine := guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			DN         string `json:"dn"`
			DigestUser string `json:"digest_user"`
			Secret     string `json:"secret"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			http.Error(w, `bad json: want {"digest_user":"…","secret":"…"[,"dn":"…"]}`, http.StatusBadRequest)
			return
		}
		id := r.PathValue("id")
		if _, ok := store.UserFor(id); !ok {
			http.NotFound(w, r)
			return
		}
		line, err := store.SetPBXLine(id, in.DN, in.DigestUser, in.Secret)
		switch {
		case errors.Is(err, ErrInvalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, ErrNoSecretKey):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		case err != nil:
			http.Error(w, "enrolment store: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if hooks.OnPBXLine != nil {
			cred, ok, err := store.PBXCredential(id)
			if err != nil || !ok {
				http.Error(w, "enrolment store: the line was saved but cannot be read back", http.StatusInternalServerError)
				return
			}
			hooks.OnPBXLine(id, &cred)
		}
		writeJSON(w, http.StatusOK, line)
	})
	mux.HandleFunc("PUT /v1/admin/devices/{id}/pbx-line", setLine)
	mux.HandleFunc("POST /v1/admin/devices/{id}/pbx-line", setLine)

	mux.HandleFunc("DELETE /v1/admin/devices/{id}/pbx-line", guard(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ok, err := store.DeletePBXLine(id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if hooks.OnPBXLine != nil {
			hooks.OnPBXLine(id, nil)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// EnrolInfo is what a claim hands the phone besides its credential.
type EnrolInfo struct {
	SignalPort int
	SIPDomain  string
	CertSHA256 string
	// Delay is imposed on every miss so guessing is slow; 500 ms by
	// default, 0 in tests.
	Delay time.Duration
	// Limit is the attempts one source address gets per minute (5).
	Limit int
	Now   func() time.Time
}

// ClaimResponse is the body of a successful POST /v1/enrol.
type ClaimResponse struct {
	DeviceID   string `json:"device_id"`
	User       string `json:"user"`
	Token      string `json:"token"`
	SignalPort int    `json:"signal_port"`
	SIPDomain  string `json:"sip_domain"`
	CertSHA256 string `json:"cert_sha256"`
}

// NewEnrolHandler serves the one unauthenticated write on the server
// (SPEC §4.8, §9 risk 14):
//
//	POST /v1/enrol  {"code"}  → ClaimResponse
//
// A miss answers 404 after a fixed delay; a source address that misses
// or hits more than Limit times a minute gets 429.
func NewEnrolHandler(store *Store, info EnrolInfo, hooks Hooks) http.Handler {
	if info.Delay == 0 && info.Now == nil {
		info.Delay = 500 * time.Millisecond
	}
	if info.Limit == 0 {
		info.Limit = 5
	}
	if info.Now == nil {
		info.Now = time.Now
	}
	lim := &limiter{limit: info.Limit, window: time.Minute, now: info.Now, seen: map[string][]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enrol", func(w http.ResponseWriter, r *http.Request) {
		if !lim.allow(sourceOf(r)) {
			http.Error(w, "too many attempts; try again in a minute", http.StatusTooManyRequests)
			return
		}
		var in struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || in.Code == "" {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		c, err := store.Claim(in.Code)
		if errors.Is(err, ErrBadCode) {
			time.Sleep(info.Delay)
			http.Error(w, "unknown or expired enrolment code", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "enrolment store: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if hooks.OnClaim != nil {
			hooks.OnClaim(c.DeviceID, c.User)
		}
		writeJSON(w, http.StatusOK, ClaimResponse{
			DeviceID: c.DeviceID, User: c.User, Token: c.Token,
			SignalPort: info.SignalPort, SIPDomain: info.SIPDomain, CertSHA256: info.CertSHA256,
		})
	})
	return mux
}

// limiter counts attempts per source in a sliding window.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	seen   map[string][]time.Time
}

func (l *limiter) allow(src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := l.seen[src][:0]
	for _, t := range l.seen[src] {
		if now.Sub(t) < l.window {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.limit {
		l.seen[src] = keep
		return false
	}
	l.seen[src] = append(keep, now)
	return true
}

func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
