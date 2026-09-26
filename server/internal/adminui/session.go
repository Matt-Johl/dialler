package adminui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Sessions is the operator's login (ADMIN-API.md §9): one password, a
// cookie that lives SessionLife from the last use, a CSRF token per
// session, and a stash for a form that was submitted after the session
// had expired, so nothing typed is lost across the re-login.
//
// It is memory only. dialler-admin keeps no state the API cannot
// re-read (rule 1 of the contract); a restart logs everyone out, which
// is the correct consequence.
type Sessions struct {
	mu       sync.Mutex
	password []byte
	now      func() time.Time
	sessions map[string]*session
	pending  map[string]*Pending
}

type session struct {
	csrf    string
	expires time.Time
}

// Pending is a form submitted on an expired session, replayed after login.
type Pending struct {
	Method  string
	Path    string
	Form    url.Values
	expires time.Time
}

const (
	// SessionLife is the cookie's life, refreshed on every use.
	SessionLife = 12 * time.Hour
	// PendingLife is how long a stashed form waits for its login.
	PendingLife = 15 * time.Minute
	cookieName  = "dialler_admin"
)

// NewSessions builds the store with the operator's password.
func NewSessions(password string) *Sessions {
	return &Sessions{password: []byte(password), now: time.Now, sessions: map[string]*session{}, pending: map[string]*Pending{}}
}

// Username is the one operator account. There are no others yet; the
// login still takes a username so adding them later changes no habit.
const Username = "admin"

// Login checks the username and password (both in constant time) and,
// when they match, opens a session and returns its cookie value.
func (s *Sessions) Login(username, password string) (id string, ok bool) {
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(Username)) == 1
	passOK := len(s.password) > 0 && subtle.ConstantTimeCompare(s.password, []byte(password)) == 1
	if !userOK || !passOK {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	id = random(24)
	s.sessions[id] = &session{csrf: random(24), expires: s.now().Add(SessionLife)}
	return id, true
}

// Logout ends a session.
func (s *Sessions) Logout(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// Check reports whether id is a live session, refreshing its expiry, and
// returns its CSRF token.
func (s *Sessions) Check(id string) (csrf string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	se := s.sessions[id]
	if se == nil || !se.expires.After(s.now()) {
		delete(s.sessions, id)
		return "", false
	}
	se.expires = s.now().Add(SessionLife)
	return se.csrf, true
}

// CSRFMatches reports whether token is the session's.
func (s *Sessions) CSRFMatches(id, token string) bool {
	csrf, ok := s.Check(id)
	return ok && token != "" && subtle.ConstantTimeCompare([]byte(csrf), []byte(token)) == 1
}

// Stash keeps a form for replay after login and returns its one-time key.
func (s *Sessions) Stash(method, path string, form url.Values) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	key := random(24)
	s.pending[key] = &Pending{Method: method, Path: path, Form: form, expires: s.now().Add(PendingLife)}
	return key
}

// Take removes and returns a stashed form.
func (s *Sessions) Take(key string) (*Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pending[key]
	delete(s.pending, key)
	if p == nil || !p.expires.After(s.now()) {
		return nil, false
	}
	return p, true
}

func (s *Sessions) sweepLocked() {
	now := s.now()
	for id, se := range s.sessions {
		if !se.expires.After(now) {
			delete(s.sessions, id)
		}
	}
	for k, p := range s.pending {
		if !p.expires.After(now) {
			delete(s.pending, k)
		}
	}
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// cookie builds the session cookie: HttpOnly, Secure, SameSite=Strict,
// path /, no Expires (a browser session cookie; the server's own expiry
// is what counts).
func cookie(id string) *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode}
}

func clearCookie() *http.Cookie {
	return &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode, MaxAge: -1}
}

func sessionID(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return c.Value
}
