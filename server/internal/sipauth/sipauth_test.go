package sipauth

import (
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// The enrolment store, as the authenticator sees it.
type secrets map[string]struct{ ha1, user string }

func (s secrets) DigestSecret(id string) (string, string, bool) {
	r, ok := s[id]
	return r.ha1, r.user, ok
}

const (
	realm = "dialler"
	dev   = "dev-a"
	tok   = "tok_dev_a_harness_fixed"
)

func newAuth(now func() time.Time) *Authenticator {
	a := New(realm, secrets{dev: {HA1(dev, realm, tok), "201"}})
	if now != nil {
		a.now = now
	}
	return a
}

func register() *sip.Request {
	return sip.NewRequest(sip.REGISTER, sip.Uri{Host: realm})
}

// answer does what baresip does with a 401: parse the challenge, compute
// the Digest response for the method/URI with the device credential, and
// put it on the request.
func answer(t *testing.T, req *sip.Request, res *sip.Response, method, uri, user, pass string) {
	t.Helper()
	chal, err := digest.ParseChallenge(res.GetHeader("WWW-Authenticate").Value())
	if err != nil {
		t.Fatal(err)
	}
	cred, err := digest.Digest(chal, digest.Options{Method: method, URI: uri, Username: user, Password: pass, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	req.RemoveHeader("Authorization")
	req.AppendHeader(sip.NewHeader("Authorization", cred.String()))
}

func TestChallengeThenVerifyWithTheDeviceCredential(t *testing.T) {
	a := newAuth(nil)
	req := register()
	if _, err := a.Verify(req); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("bare request: %v, want ErrNoCredentials", err)
	}
	res := a.Challenge(req, false)
	if res.StatusCode != 401 {
		t.Fatalf("challenge status %d", res.StatusCode)
	}
	answer(t, req, res, "REGISTER", "sip:"+realm, dev, tok)
	user, err := a.Verify(req)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if user != "201" {
		t.Fatalf("enrolled user %q, want 201", user)
	}
	// The same nonce is good again within its window (baresip re-uses it).
	if _, err := a.Verify(req); err != nil {
		t.Fatalf("second use of a fresh nonce: %v", err)
	}
}

func TestWrongPasswordUnknownDeviceAndWrongMethodAreRefused(t *testing.T) {
	a := newAuth(nil)
	req := register()
	res := a.Challenge(req, false)

	answer(t, req, res, "REGISTER", "sip:"+realm, dev, "not-the-token")
	if _, err := a.Verify(req); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: %v, want ErrBadCredentials", err)
	}
	answer(t, req, res, "REGISTER", "sip:"+realm, "dev-z", tok)
	if _, err := a.Verify(req); !errors.Is(err, ErrUnknownDevice) {
		t.Fatalf("unknown device: %v, want ErrUnknownDevice", err)
	}
	// Credentials computed for INVITE do not verify a REGISTER (method is
	// part of the digest), so a captured response cannot be replayed on a
	// different request.
	answer(t, req, res, "INVITE", "sip:"+realm, dev, tok)
	if _, err := a.Verify(req); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("method mismatch: %v, want ErrBadCredentials", err)
	}
}

func TestStaleAndForgedNoncesAreChallengedAgain(t *testing.T) {
	now := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	a := newAuth(func() time.Time { return now })
	req := register()
	res := a.Challenge(req, false)
	answer(t, req, res, "REGISTER", "sip:"+realm, dev, tok)

	now = now.Add(NonceTTL + time.Second)
	if _, err := a.Verify(req); !errors.Is(err, ErrStale) {
		t.Fatalf("expired nonce: %v, want ErrStale", err)
	}
	stale := a.Challenge(req, true)
	if v := stale.GetHeader("WWW-Authenticate").Value(); !contains(v, "stale=true") {
		t.Fatalf("re-challenge must say stale=true: %s", v)
	}

	forged := register()
	forged.AppendHeader(sip.NewHeader("Authorization",
		`Digest username="dev-a", realm="dialler", nonce="0000", uri="sip:dialler", response="00", qop=auth, nc=00000001, cnonce="x"`))
	if _, err := a.Verify(forged); !errors.Is(err, ErrStale) {
		t.Fatalf("nonce we never issued: %v, want ErrStale", err)
	}
}

func TestOtherRealmOrAlgorithmIsRefused(t *testing.T) {
	a := newAuth(nil)
	req := register()
	res := a.Challenge(req, false)
	answer(t, req, res, "REGISTER", "sip:"+realm, dev, tok)
	v := req.GetHeader("Authorization").Value()
	req.RemoveHeader("Authorization")
	req.AppendHeader(sip.NewHeader("Authorization", replace(v, `realm="dialler"`, `realm="other"`)))
	if _, err := a.Verify(req); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("other realm: %v, want ErrBadCredentials", err)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func replace(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}
