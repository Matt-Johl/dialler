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
	"strings"
	"sync"
	"time"

	"dialler/server/internal/sipauth"
)

// Device is the public view of an enrolled device.
type Device struct {
	DeviceID string `json:"device_id"`
	User     string `json:"user"`
	// Label is the operator's name for the phone ("Matt's iPhone"); the
	// device id is a credential username nobody should have to read.
	Label    string    `json:"label,omitempty"`
	IssuedAt time.Time `json:"issued_at"`
	Revoked  bool      `json:"revoked"`
	// Enrolled is whether the device holds a credential at all: false
	// between its creation and the claim of its enrolment code.
	Enrolled bool `json:"enrolled"`
	// CodePending is whether an unexpired enrolment code is outstanding.
	CodePending bool `json:"code_pending,omitempty"`
	// Config is the device's server-managed settings; nil until set.
	Config *DeviceConfig `json:"config,omitempty"`
}

type record struct {
	User  string `json:"user"`
	Label string `json:"label,omitempty"`
	// TokenHash is the hex sha256 of the bearer token; "" until a
	// credential has been issued (a device waiting for its first claim).
	TokenHash string `json:"token_hash"`
	// HA1 is the SIP Digest form of the same token, MD5(device:realm:token)
	// (sipauth.HA1): what the app-leg registrar verifies against. Empty for
	// credentials issued before a realm was configured; re-issue those.
	HA1      string    `json:"ha1,omitempty"`
	IssuedAt time.Time `json:"issued_at"`
	Revoked  bool      `json:"revoked"`
	// The outstanding enrolment code (SPEC §4.8), stored only as its hex
	// sha256, and when it stops being claimable.
	CodeHash    string    `json:"code_hash,omitempty"`
	CodeExpires time.Time `json:"code_expires,omitempty"`
	// Server-managed settings (SPEC §6 item 8b): the Wi-Fi SSIDs the
	// device's Local Push provider runs on. ConfigVersion is 0 until an
	// administrator has set anything, and the app then keeps its own.
	SSIDs         []string `json:"ssids,omitempty"`
	ConfigVersion int64    `json:"config_version,omitempty"`
}

// DeviceConfig is the device's server-managed settings as the admin API
// and the wire protocol carry them.
type DeviceConfig struct {
	Version int64    `json:"version"`
	SSIDs   []string `json:"ssids"`
}

// CodeTTL is how long an enrolment code can be claimed.
const CodeTTL = 15 * time.Minute

// codeAlphabet is Crockford base32 without I, L, O and U: nothing that
// reads as something else when typed from a screen.
const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// ErrBadCode is returned for an unknown, used or expired enrolment code.
var ErrBadCode = errors.New("enroll: unknown or expired enrolment code")

// Claimed is what a device gets for a valid enrolment code.
type Claimed struct {
	DeviceID string
	User     string
	Token    string
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
	if old, ok := s.devices[deviceID]; ok {
		rec.Label = old.Label
	}
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

// Create registers a device without a credential: it exists, bound to
// user, until an enrolment code is claimed for it (SPEC §4.8). An empty
// deviceID is generated ("dev_" + six code characters). An existing
// device keeps its credential; only the label and user are updated.
func (s *Store) Create(deviceID, user, label string) (string, error) {
	if user == "" {
		return "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if deviceID == "" {
		for {
			deviceID = "dev_" + randomCode(6)
			if _, taken := s.devices[deviceID]; !taken {
				break
			}
		}
	}
	rec, ok := s.devices[deviceID]
	if !ok {
		rec = &record{IssuedAt: s.now()}
		s.devices[deviceID] = rec
	}
	rec.User, rec.Label = user, label
	return deviceID, s.saveLocked()
}

// Update changes a device's extension and label, keeping its credential,
// code and settings. ErrInvalid for an unknown device or an empty user.
func (s *Store) Update(deviceID, user, label string) error {
	if user == "" {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.devices[deviceID]
	if !ok {
		return ErrInvalid
	}
	rec.User, rec.Label = user, label
	return s.saveLocked()
}

// MintCode issues a fresh enrolment code for deviceID, replacing any
// outstanding one: eight characters, claimable for CodeTTL, stored only
// as a hash. The device may be revoked — claiming the code brings it back
// with a new credential.
func (s *Store) MintCode(deviceID string) (code string, expires time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.devices[deviceID]
	if !ok {
		return "", time.Time{}, ErrInvalid
	}
	code = randomCode(8)
	rec.CodeHash = hashToken(code)
	rec.CodeExpires = s.now().Add(CodeTTL)
	return code, rec.CodeExpires, s.saveLocked()
}

// Claim trades an enrolment code for a fresh credential: the code is
// spent, the device's previous token (if any) stops working, and a
// revoked device is live again. Every record is compared in constant time
// whether or not one has matched, so the answer's timing says nothing.
func (s *Store) Claim(code string) (Claimed, error) {
	want := []byte(hashToken(NormalizeCode(code)))
	s.mu.Lock()
	defer s.mu.Unlock()
	var id string
	var rec *record
	now := s.now()
	for d, r := range s.devices {
		if r.CodeHash == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(r.CodeHash), want) == 1 && now.Before(r.CodeExpires) {
			id, rec = d, r
		}
	}
	if rec == nil {
		return Claimed{}, ErrBadCode
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Claimed{}, err
	}
	tok := "tok_" + base64.RawURLEncoding.EncodeToString(b[:])
	rec.TokenHash = hashToken(tok)
	rec.HA1 = ""
	if s.Realm != "" {
		rec.HA1 = sipauth.HA1(id, s.Realm, tok)
	}
	rec.IssuedAt = now
	rec.Revoked = false
	rec.CodeHash, rec.CodeExpires = "", time.Time{}
	if err := s.saveLocked(); err != nil {
		return Claimed{}, err
	}
	return Claimed{DeviceID: id, User: rec.User, Token: tok}, nil
}

// Config returns deviceID's server-managed settings, or nil when none
// have been set (the app then keeps its own).
func (s *Store) Config(deviceID string) *DeviceConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.devices[deviceID]
	if !ok || r.ConfigVersion == 0 {
		return nil
	}
	return &DeviceConfig{Version: r.ConfigVersion, SSIDs: append([]string{}, r.SSIDs...)}
}

// SetConfig replaces deviceID's SSID list (trimmed, empties and repeats
// dropped, order kept) and bumps its config version — even for the same
// list, so an administrator's "apply" always reaches the phone. Returns
// the new settings; ErrInvalid for an unknown device.
func (s *Store) SetConfig(deviceID string, ssids []string) (DeviceConfig, error) {
	clean := make([]string, 0, len(ssids))
	seen := map[string]bool{}
	for _, ss := range ssids {
		ss = strings.TrimSpace(ss)
		if ss == "" || seen[ss] {
			continue
		}
		seen[ss] = true
		clean = append(clean, ss)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.devices[deviceID]
	if !ok {
		return DeviceConfig{}, ErrInvalid
	}
	r.SSIDs = clean
	r.ConfigVersion++
	if err := s.saveLocked(); err != nil {
		return DeviceConfig{}, err
	}
	return DeviceConfig{Version: r.ConfigVersion, SSIDs: append([]string{}, clean...)}, nil
}

// NormalizeCode makes a typed code comparable: upper case, separators
// dropped, and the letters the alphabet leaves out mapped to the digits
// they are mistaken for (I and L → 1, O → 0).
func NormalizeCode(code string) string {
	out := make([]byte, 0, len(code))
	for _, c := range strings.ToUpper(code) {
		switch {
		case c == ' ' || c == '-':
		case c == 'I' || c == 'L':
			out = append(out, '1')
		case c == 'O':
			out = append(out, '0')
		case c < 128:
			out = append(out, byte(c))
		}
	}
	return string(out)
}

func randomCode(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = codeAlphabet[int(b[i])%len(codeAlphabet)]
	}
	return string(b)
}

// Authenticate satisfies gateway.Authenticator.
func (s *Store) Authenticate(_ context.Context, deviceID, token string) (bool, error) {
	s.mu.RLock()
	r, ok := s.devices[deviceID]
	s.mu.RUnlock()
	if !ok || r.Revoked || r.TokenHash == "" {
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

// Delete removes the device's record outright (SPEC §4.8 "purge"): its
// credential, code and settings are gone and the id may be enrolled
// afresh. Returns false if unknown.
func (s *Store) Delete(deviceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.devices[deviceID]; !ok {
		return false, nil
	}
	delete(s.devices, deviceID)
	return true, s.saveLocked()
}

// exists reports whether a record exists at all, revoked or not.
func (s *Store) exists(deviceID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.devices[deviceID]
	return ok
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
	now := s.now()
	for id, r := range s.devices {
		d := Device{
			DeviceID: id, User: r.User, Label: r.Label, IssuedAt: r.IssuedAt, Revoked: r.Revoked,
			Enrolled:    r.TokenHash != "",
			CodePending: r.CodeHash != "" && now.Before(r.CodeExpires),
		}
		if r.ConfigVersion > 0 {
			d.Config = &DeviceConfig{Version: r.ConfigVersion, SSIDs: append([]string{}, r.SSIDs...)}
		}
		out = append(out, d)
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
