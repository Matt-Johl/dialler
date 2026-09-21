package adminui

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The operator's password is kept as PBKDF2-SHA256 in a file the admin
// reads at start-up:
//
//	pbkdf2-sha256$<iterations>$<salt, base64>$<hash, base64>
//
// Standard library only (crypto/pbkdf2), with an iteration count that
// makes a guess cost tens of milliseconds and a login the same — nobody
// logs in often enough to notice, and a dictionary run does.
const (
	hashScheme     = "pbkdf2-sha256"
	hashIterations = 600_000
	hashLength     = 32
)

// HashPassword returns the stored form of password.
func HashPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, hashLength)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, hashIterations,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the stored hash, in
// constant time over the hash.
func VerifyPassword(stored, password string) bool {
	parts := strings.Split(strings.TrimSpace(stored), "$")
	if len(parts) != 4 || parts[0] != hashScheme {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// A session is one signed-in browser: a random cookie value, a CSRF token
// every form carries, an idle expiry, and a one-time flash the next page
// shows (an enrolment code, a pending CSV upload, a notice).
type session struct {
	csrf    string
	expires time.Time
	flash   map[string]any
}

const (
	sessionCookie = "dialler_admin"
	sessionIdle   = 12 * time.Hour
)

type sessions struct {
	mu   sync.Mutex
	now  func() time.Time
	byID map[string]*session
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{now: now, byID: map[string]*session{}}
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// start opens a session and returns its cookie value.
func (s *sessions) start() (string, *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := randomToken()
	sess := &session{csrf: randomToken(), expires: s.now().Add(sessionIdle), flash: map[string]any{}}
	s.byID[id] = sess
	return id, sess
}

// get returns the live session for a cookie value, touching its expiry.
func (s *sessions) get(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		return nil
	}
	now := s.now()
	if !now.Before(sess.expires) {
		delete(s.byID, id)
		return nil
	}
	sess.expires = now.Add(sessionIdle)
	return sess
}

func (s *sessions) end(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// setFlash stores a value the next request for key takes away.
func (s *sessions) setFlash(sess *session, key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.flash[key] = v
}

func (s *sessions) takeFlash(sess *session, key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := sess.flash[key]
	delete(sess.flash, key)
	return v
}

func (s *sessions) csrf(sess *session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sess.csrf
}

// loginLimiter slows password guessing: five attempts a minute per source.
type loginLimiter struct {
	mu   sync.Mutex
	now  func() time.Time
	seen map[string][]time.Time
}

func (l *loginLimiter) allow(src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	keep := l.seen[src][:0]
	for _, t := range l.seen[src] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= 5 {
		l.seen[src] = keep
		return false
	}
	l.seen[src] = append(keep, now)
	return true
}

func sourceOf(r *http.Request) string {
	if i := strings.LastIndex(r.RemoteAddr, ":"); i > 0 {
		return r.RemoteAddr[:i]
	}
	return r.RemoteAddr
}
