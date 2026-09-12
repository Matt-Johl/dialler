// Package enroll is the device enrolment store: it issues per-device
// credentials, verifies them for the gateway and HTTP APIs, and maps each
// device to its SIP user. Persistence is a single JSON file; the store is
// small (one record per phone) so this is deliberately simple.
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"dialler/server/internal/sipauth"
)

// Device is the public view of an enrolled device.
type Device struct {
	DeviceID string    `json:"device_id"`
	User     string    `json:"user"`
	IssuedAt time.Time `json:"issued_at"`
	Revoked  bool      `json:"revoked"`
}

type record struct {
	User      string `json:"user"`
	TokenHash string `json:"token_hash"` // hex sha256
	// HA1 is the SIP Digest form of the same token, MD5(device:realm:token)
	// (sipauth.HA1): what the app-leg registrar verifies against. Empty for
	// credentials issued before a realm was configured; re-issue those.
	HA1      string    `json:"ha1,omitempty"`
	IssuedAt time.Time `json:"issued_at"`
	Revoked  bool      `json:"revoked"`
}

// Store is safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	path    string // "" → memory only
	devices map[string]*record
	now     func() time.Time
	// Realm is the SIP Digest realm (the server's SIP domain) that HA1 is
	// computed for at issue time. Set it before issuing credentials.
	Realm string
}

// ErrInvalid is returned for empty device ids or users.
var ErrInvalid = errors.New("enroll: device_id and user are required")

// Open loads the store at path, creating it on first save. An empty path
// gives an in-memory store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, devices: map[string]*record{}, now: time.Now}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("enroll: read %s: %w", path, err)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.devices); err != nil {
			return nil, fmt.Errorf("enroll: parse %s: %w", path, err)
		}
	}
	return s, nil
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Issue creates (or re-issues, replacing) the credential for deviceID and
// binds it to user. The plaintext token is returned once and never stored.
func (s *Store) Issue(deviceID, user string) (string, error) {
	return s.IssueToken(deviceID, user, "")
}

// IssueToken is Issue with a caller-chosen token (min 16 chars). Meant for
// development fixtures so a re-provisioned harness keeps the same tokens;
// "" generates a random one. Only the hash is stored either way.
func (s *Store) IssueToken(deviceID, user, token string) (string, error) {
	if deviceID == "" || user == "" {
		return "", ErrInvalid
	}
	tok := token
	if tok == "" {
		var b [32]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		tok = "tok_" + base64.RawURLEncoding.EncodeToString(b[:])
	} else if len(tok) < 16 {
		return "", ErrInvalid
	}
	s.mu.Lock()
	rec := &record{User: user, TokenHash: hashToken(tok), IssuedAt: s.now()}
	if s.Realm != "" {
		rec.HA1 = sipauth.HA1(deviceID, s.Realm, tok)
	}
	s.devices[deviceID] = rec
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	return tok, nil
}

// Authenticate satisfies gateway.Authenticator.
func (s *Store) Authenticate(_ context.Context, deviceID, token string) (bool, error) {
	s.mu.RLock()
	r, ok := s.devices[deviceID]
	s.mu.RUnlock()
	if !ok || r.Revoked {
		return false, nil
	}
	want := []byte(r.TokenHash)
	got := []byte(hashToken(token))
	return subtle.ConstantTimeCompare(want, got) == 1, nil
}

// Revoke disables the device's credential. Returns false if unknown.
func (s *Store) Revoke(deviceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.devices[deviceID]
	if !ok {
		return false, nil
	}
	r.Revoked = true
	return true, s.saveLocked()
}

// UserFor returns the SIP user bound to an enrolled, non-revoked device.
func (s *Store) UserFor(deviceID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.devices[deviceID]
	if !ok || r.Revoked {
		return "", false
	}
	return r.User, true
}

// DigestSecret satisfies sipauth.Secrets: the SIP Digest H(A1) and enrolled
// user of a live device. ok is false for unknown or revoked devices and for
// credentials issued before a realm was configured.
func (s *Store) DigestSecret(deviceID string) (ha1, user string, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, found := s.devices[deviceID]
	if !found || r.Revoked || r.HA1 == "" {
		return "", "", false
	}
	return r.HA1, r.User, true
}

// Devices lists every record, sorted by device id.
func (s *Store) Devices() []Device {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Device, 0, len(s.devices))
	for id, r := range s.devices {
		out = append(out, Device{DeviceID: id, User: r.User, IssuedAt: r.IssuedAt, Revoked: r.Revoked})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceID < out[j].DeviceID })
	return out
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.devices, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
