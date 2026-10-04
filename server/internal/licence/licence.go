// Package licence holds the product licence: X app devices until a date,
// vendor-signed, bound to one server installation (SPEC §4.9).
//
// A licence is a signed document, not an encrypted one. Its payload is plain
// base64 JSON that anyone can read; what makes it a licence is the Ed25519
// signature on the end, which only the vendor's private key can produce and
// which the public key compiled into this binary (vendorkey.go) and into the
// app can check. Change one character of the payload and the check fails.
//
// This package owns the format and the state: parse and verify a token,
// bind it to the install id, know when it expires, say what it is. The
// seats it grants are counted by the device store (enroll), and the doors
// an unlicensed device is refused at are the gateway's, the SIP leg's and
// the line manager's. The app verifies the same token with the same key and
// its own clock, which is what makes expiry hold on a server the customer
// controls.
package licence

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Prefix names the token format; a future format gets a new one.
const Prefix = "DL1."

// MaxLen bounds a token a caller will look at: a licence is a few hundred
// bytes, and nothing legitimate is anywhere near this.
const MaxLen = 4096

// WarnWindow is how long before expiry the licence counts as expiring: the
// admin UI and the log warn, the app shows nothing until its own last week.
const WarnWindow = 30 * 24 * time.Hour

// PublicKey is the vendor's verification key. vendorkey.go sets it at build
// time; tests overwrite it with a key of their own. Nil verifies nothing,
// so a binary built without the key accepts no licence rather than any.
var PublicKey ed25519.PublicKey

// Licence is the signed payload.
type Licence struct {
	V          int       `json:"v"`
	ID         string    `json:"id"`
	Customer   string    `json:"customer"`
	InstallID  string    `json:"install_id"`
	Seats      int       `json:"seats"`
	IssuedAt   time.Time `json:"issued_at"`
	ValidUntil time.Time `json:"valid_until"`
}

// The reasons a token is refused, in the order they are checked. Each is
// wrapped with the detail the operator needs; errors.Is finds the kind.
var (
	// ErrFormat: not a token at all — prefix, parts, encoding, length.
	ErrFormat = errors.New("licence: not a licence token")
	// ErrSignature: a token whose signature is not the vendor's over this
	// payload. An edited licence lands here.
	ErrSignature = errors.New("licence: signature does not verify")
	// ErrPayload: signed, but says something this binary does not accept.
	ErrPayload = errors.New("licence: payload is not valid")
	// ErrWrongInstall: issued for another server.
	ErrWrongInstall = errors.New("licence: issued for another install id")
	// ErrExpired: valid_until has passed.
	ErrExpired = errors.New("licence: expired")
)

// Parse verifies a token's signature with PublicKey and returns its payload.
// It does not check the install id or the date; Check does, because those
// depend on where and when the question is asked.
func Parse(token string) (Licence, error) {
	if len(token) > MaxLen {
		return Licence{}, fmt.Errorf("%w: longer than %d bytes", ErrFormat, MaxLen)
	}
	if !strings.HasPrefix(token, Prefix) {
		return Licence{}, fmt.Errorf("%w: expected a %q token", ErrFormat, strings.TrimSuffix(Prefix, "."))
	}
	rest := strings.TrimPrefix(token, Prefix)
	parts := strings.Split(rest, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Licence{}, fmt.Errorf("%w: expected payload and signature", ErrFormat)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Licence{}, fmt.Errorf("%w: payload is not base64url", ErrFormat)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Licence{}, fmt.Errorf("%w: signature is not base64url", ErrFormat)
	}
	if len(sig) != ed25519.SignatureSize {
		return Licence{}, fmt.Errorf("%w: signature is %d bytes, want %d", ErrFormat, len(sig), ed25519.SignatureSize)
	}
	if len(PublicKey) != ed25519.PublicKeySize {
		return Licence{}, fmt.Errorf("%w: no vendor key compiled into this build", ErrSignature)
	}
	// The signature covers the ASCII the verifier holds, prefix included,
	// so nothing is re-encoded on either side before checking.
	if !ed25519.Verify(PublicKey, []byte(Prefix+parts[0]), sig) {
		return Licence{}, ErrSignature
	}
	var l Licence
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return Licence{}, fmt.Errorf("%w: %v", ErrPayload, err)
	}
	if dec.More() {
		return Licence{}, fmt.Errorf("%w: trailing data", ErrPayload)
	}
	if err := l.validate(); err != nil {
		return Licence{}, err
	}
	return l, nil
}

// validate is the grammar: what a signed payload must say to be a licence
// this binary understands.
func (l Licence) validate() error {
	switch {
	case l.V != 1:
		return fmt.Errorf("%w: version %d, this server understands 1", ErrPayload, l.V)
	case l.ID == "":
		return fmt.Errorf("%w: no id", ErrPayload)
	case l.Customer == "":
		return fmt.Errorf("%w: no customer", ErrPayload)
	case l.InstallID == "":
		return fmt.Errorf("%w: no install id", ErrPayload)
	case l.Seats < 0:
		return fmt.Errorf("%w: %d seats", ErrPayload, l.Seats)
	case l.IssuedAt.IsZero() || l.ValidUntil.IsZero():
		return fmt.Errorf("%w: issued_at and valid_until are required", ErrPayload)
	case !l.ValidUntil.After(l.IssuedAt):
		return fmt.Errorf("%w: valid_until is not after issued_at", ErrPayload)
	}
	return nil
}

// Sign produces a token for l with the vendor's private key. The tool uses
// it; tests use it with a key of their own. It refuses what Parse would.
func Sign(priv ed25519.PrivateKey, l Licence) (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("licence: private key is not an Ed25519 key")
	}
	l.IssuedAt, l.ValidUntil = l.IssuedAt.UTC(), l.ValidUntil.UTC()
	payload, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(Prefix+b64))
	return Prefix + b64 + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Check is whether this licence applies here and now: issued for installID
// and not yet expired. The wrong install is reported first, because that is
// the operator's problem whatever the date says.
func (l Licence) Check(installID string, now time.Time) error {
	if l.InstallID != installID {
		return fmt.Errorf("%w: issued for install id %s, this server is %s", ErrWrongInstall, l.InstallID, installID)
	}
	if !now.Before(l.ValidUntil) {
		return fmt.Errorf("%w on %s", ErrExpired, l.ValidUntil.UTC().Format("2006-01-02"))
	}
	return nil
}

// State is what a licence is doing, for the summary and the log.
type State string

const (
	// StateMissing: no licence installed. Zero seats.
	StateMissing State = "missing"
	// StateInvalid: a file is present but is not a verifiable licence.
	StateInvalid State = "invalid"
	// StateWrongInstall: a valid licence for another server. Zero seats.
	StateWrongInstall State = "wrong_install"
	// StateActive: in force.
	StateActive State = "active"
	// StateExpiring: in force, inside the last WarnWindow.
	StateExpiring State = "expiring"
	// StateExpired: was in force; now zero seats.
	StateExpired State = "expired"
)

// StateAt is the licence's state by date alone (the install id having been
// checked by whoever holds it).
func (l Licence) StateAt(now time.Time) State {
	switch {
	case !now.Before(l.ValidUntil):
		return StateExpired
	case now.Add(WarnWindow).After(l.ValidUntil):
		return StateExpiring
	}
	return StateActive
}

// DaysLeft is the number of whole days until expiry, 0 once it has passed.
func (l Licence) DaysLeft(now time.Time) int {
	d := l.ValidUntil.Sub(now)
	if d <= 0 {
		return 0
	}
	return int(d / (24 * time.Hour))
}

// Summary is the licence as the admin API reports it. It never carries the
// token itself; that is on disk and in the welcome.
type Summary struct {
	State      State     `json:"state"`
	InstallID  string    `json:"install_id"`
	ID         string    `json:"id,omitempty"`
	Customer   string    `json:"customer,omitempty"`
	Seats      int       `json:"seats"`
	SeatsUsed  int       `json:"seats_used"`
	IssuedAt   time.Time `json:"issued_at,omitzero"`
	ValidUntil time.Time `json:"valid_until,omitzero"`
	DaysLeft   int       `json:"days_left"`
	// Error is why the stored licence is unusable, when it is.
	Error string `json:"error,omitempty"`
}
