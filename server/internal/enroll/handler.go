package enroll

import (
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
	// OnPurge: the admin deleted a device's record outright; whatever else
	// the device owned (directory, diagnostics, line, sessions) goes too.
	OnPurge func(deviceID string)
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
