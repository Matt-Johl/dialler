// Package sipauth authenticates the app leg with SIP Digest (RFC 3261 §22,
// RFC 2617) using the device's enrolment credential: username = device id,
// password = device token, realm = the server's SIP domain. The server never
// holds the token; it keeps H(A1) = MD5(device:realm:token), which is all
// Digest verification needs (enroll.Store.DigestSecret).
//
// The app leg is TLS, so Digest here is not about eavesdroppers: it binds a
// SIP registration or call to an enrolled device, so that a host on the
// same Wi-Fi cannot register as somebody else and take their calls.
package sipauth

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// Secrets looks up a device's Digest secret. Implemented by enroll.Store.
type Secrets interface {
	// DigestSecret returns H(A1) and the SIP user the device is enrolled
	// for; ok is false for unknown or revoked devices.
	DigestSecret(deviceID string) (ha1, user string, ok bool)
}

var (
	// ErrNoCredentials: the request carries no Digest Authorization; challenge it.
	ErrNoCredentials = errors.New("sipauth: no Digest credentials")
	// ErrStale: the nonce is unknown or expired; challenge again with stale=true.
	ErrStale = errors.New("sipauth: stale or unknown nonce")
	// ErrBadCredentials: the response does not verify; refuse.
	ErrBadCredentials = errors.New("sipauth: bad credentials")
	// ErrUnknownDevice: no live enrolment for the username; refuse.
	ErrUnknownDevice = errors.New("sipauth: unknown or revoked device")
)

// NonceTTL is how long a challenge's nonce stays acceptable. A nonce may be
// reused within it (baresip re-uses one with an increasing nonce count);
// there is no nonce-count tracking, which is fine for a LAN app leg.
const NonceTTL = 5 * time.Minute

// HA1 is RFC 2617's H(A1) for MD5: the stored form of a Digest credential.
func HA1(username, realm, password string) string {
	return md5hex(username + ":" + realm + ":" + password)
}

// Authenticator challenges and verifies requests for one realm.
type Authenticator struct {
	realm   string
	secrets Secrets
	now     func() time.Time

	mu     sync.Mutex
	nonces map[string]time.Time // nonce → issued at
}

func New(realm string, secrets Secrets) *Authenticator {
	return &Authenticator{realm: realm, secrets: secrets, now: time.Now, nonces: map[string]time.Time{}}
}

// Realm is the Digest realm, the server's SIP domain.
func (a *Authenticator) Realm() string { return a.realm }

// Challenge builds the 401 for a request with no (or stale) credentials.
func (a *Authenticator) Challenge(req *sip.Request, stale bool) *sip.Response {
	res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
	v := fmt.Sprintf(`Digest realm="%s", nonce="%s", algorithm=MD5, qop="auth"`, a.realm, a.newNonce())
	if stale {
		v += ", stale=true"
	}
	res.AppendHeader(sip.NewHeader("WWW-Authenticate", v))
	return res
}

// Verify checks the request's Authorization header against the enrolment
// store. On success it returns the SIP user the device is enrolled for; the
// caller decides whether the request's own user matches.
func (a *Authenticator) Verify(req *sip.Request) (string, error) {
	h := req.GetHeader("Authorization")
	if h == nil || !digest.IsDigest(h.Value()) {
		return "", ErrNoCredentials
	}
	c, err := digest.ParseCredentials(h.Value())
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadCredentials, err)
	}
	if !strings.EqualFold(c.Realm, a.realm) {
		return "", fmt.Errorf("%w: realm %q", ErrBadCredentials, c.Realm)
	}
	if c.Algorithm != "" && !strings.EqualFold(c.Algorithm, "MD5") {
		return "", fmt.Errorf("%w: algorithm %q", ErrBadCredentials, c.Algorithm)
	}
	if c.QOP != "" && c.QOP != "auth" {
		return "", fmt.Errorf("%w: qop %q", ErrBadCredentials, c.QOP)
	}
	if !a.nonceValid(c.Nonce) {
		return "", ErrStale
	}
	ha1, user, ok := a.secrets.DigestSecret(c.Username)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownDevice, c.Username)
	}
	want := response(ha1, string(req.Method), c)
	if subtle.ConstantTimeCompare([]byte(want), []byte(strings.ToLower(c.Response))) != 1 {
		return "", fmt.Errorf("%w for device %q", ErrBadCredentials, c.Username)
	}
	return user, nil
}

// response is the expected Digest response for a request (RFC 2617 §3.2.2),
// with or without qop=auth.
func response(ha1, method string, c *digest.Credentials) string {
	ha2 := md5hex(method + ":" + c.URI)
	if c.QOP == "" {
		return md5hex(ha1 + ":" + c.Nonce + ":" + ha2)
	}
	return md5hex(fmt.Sprintf("%s:%s:%08x:%s:%s:%s", ha1, c.Nonce, c.Nc, c.Cnonce, c.QOP, ha2))
}

func (a *Authenticator) newNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	n := hex.EncodeToString(b[:])
	now := a.now()
	a.mu.Lock()
	for k, issued := range a.nonces { // keep the table bounded
		if now.Sub(issued) > NonceTTL {
			delete(a.nonces, k)
		}
	}
	a.nonces[n] = now
	a.mu.Unlock()
	return n
}

func (a *Authenticator) nonceValid(n string) bool {
	a.mu.Lock()
	issued, ok := a.nonces[n]
	a.mu.Unlock()
	return ok && a.now().Sub(issued) <= NonceTTL
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
