package licence

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// The tests sign with a key of their own and install its public half as the
// vendor key, so nothing here depends on the real key being compiled in.
var testPriv ed25519.PrivateKey

func TestMain(m *testing.M) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	testPriv = priv
	PublicKey = pub
	os.Exit(m.Run())
}

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func sample() Licence {
	return Licence{
		V:          1,
		ID:         "lic_TESTX001",
		Customer:   "Example Ltd",
		InstallID:  "0123456789abcdef0123456789abcdef",
		Seats:      10,
		IssuedAt:   t0,
		ValidUntil: t0.Add(365 * 24 * time.Hour),
	}
}

func mustSign(t *testing.T, l Licence) string {
	t.Helper()
	tok, err := Sign(testPriv, l)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestRoundTrip(t *testing.T) {
	tok := mustSign(t, sample())
	if !strings.HasPrefix(tok, "DP1.") || strings.Count(tok, ".") != 2 {
		t.Fatalf("token shape: %q", tok)
	}
	got, err := Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	want := sample()
	if got.ID != want.ID || got.Customer != want.Customer || got.InstallID != want.InstallID || got.Seats != want.Seats ||
		!got.IssuedAt.Equal(want.IssuedAt) || !got.ValidUntil.Equal(want.ValidUntil) || got.V != 1 {
		t.Fatalf("round trip changed the licence: %+v", got)
	}
	if err := got.Check(want.InstallID, t0.Add(time.Hour)); err != nil {
		t.Fatalf("a fresh licence for this install must check: %v", err)
	}
}

// Every part of the token is covered by the signature or the grammar: no
// edit survives.
func TestTamperedTokensAreRefused(t *testing.T) {
	tok := mustSign(t, sample())
	parts := strings.SplitN(tok, ".", 3)
	payload, sig := parts[1], parts[2]
	decoded, _ := base64.RawURLEncoding.DecodeString(payload)

	alter := func(from, to string) string {
		s := strings.Replace(string(decoded), from, to, 1)
		if s == string(decoded) {
			t.Fatalf("field %q not in payload %s", from, decoded)
		}
		return "DP1." + base64.RawURLEncoding.EncodeToString([]byte(s)) + "." + sig
	}
	cases := map[string]struct {
		tok  string
		want error
	}{
		"empty":             {"", ErrFormat},
		"wrong prefix":      {"DP2." + payload + "." + sig, ErrFormat},
		"two parts":         {"DP1." + payload, ErrFormat},
		"four parts":        {tok + ".x", ErrFormat},
		"bad base64":        {"DP1.!!!." + sig, ErrFormat},
		"short signature":   {"DP1." + payload + "." + sig[:10], ErrFormat},
		"flipped signature": {"DP1." + payload + "." + flip(sig), ErrSignature},
		"seats altered":     {alter(`"seats":10`, `"seats":500`), ErrSignature},
		"expiry altered":    {alter(`2027-10-03`, `2037-10-03`), ErrSignature},
		"install altered":   {alter(`0123456789abcdef0123456789abcdef`, `ffffffffffffffffffffffffffffffff`), ErrSignature},
		"id altered":        {alter(`lic_TESTX001`, `lic_TESTX002`), ErrSignature},
		"customer altered":  {alter(`Example Ltd`, `Another Ltd`), ErrSignature},
		"too long":          {"DP1." + strings.Repeat("A", 5000) + "." + sig, ErrFormat},
	}
	for name, c := range cases {
		if _, err := Parse(c.tok); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// A payload the signature covers can still be nonsense; the grammar is
// checked after the signature, and refuses what a future format might mean.
// Sign refuses these itself (TestSignRefusesAnInvalidLicence), so they are
// built from raw JSON, which is what a hostile or future issuer would send.
func TestPayloadGrammar(t *testing.T) {
	base := `"id":"lic_X","customer":"C","install_id":"i","seats":1,"issued_at":"2026-10-03T12:00:00Z","valid_until":"2027-10-03T12:00:00Z"`
	cases := map[string]string{
		"v 2":                 `{"v":2,` + base + `}`,
		"no v":                `{` + base + `}`,
		"negative seats":      `{"v":1,` + strings.Replace(base, `"seats":1`, `"seats":-1`, 1) + `}`,
		"empty id":            `{"v":1,` + strings.Replace(base, `"id":"lic_X"`, `"id":""`, 1) + `}`,
		"empty install":       `{"v":1,` + strings.Replace(base, `"install_id":"i"`, `"install_id":""`, 1) + `}`,
		"empty customer":      `{"v":1,` + strings.Replace(base, `"customer":"C"`, `"customer":""`, 1) + `}`,
		"expiry before issue": `{"v":1,` + strings.Replace(base, `"valid_until":"2027-10-03T12:00:00Z"`, `"valid_until":"2026-10-03T11:00:00Z"`, 1) + `}`,
		"missing dates":       `{"v":1,"id":"lic_X","customer":"C","install_id":"i","seats":1}`,
		"unknown field":       `{"v":1,` + base + `,"extra":true}`,
		"not json":            `not json`,
		"trailing data":       `{"v":1,` + base + `} {}`,
	}
	for name, raw := range cases {
		if _, err := Parse(signRaw(raw)); !errors.Is(err, ErrPayload) {
			t.Errorf("%s: err = %v, want ErrPayload", name, err)
		}
	}
	// The same shape with nothing wrong parses, so the cases above fail for
	// the stated reason and not for the way they were built.
	if _, err := Parse(signRaw(`{"v":1,` + base + `}`)); err != nil {
		t.Fatalf("the baseline payload must parse: %v", err)
	}
}

// signRaw signs an arbitrary payload, for the grammar tests.
func signRaw(payload string) string {
	b64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sig := ed25519.Sign(testPriv, []byte("DP1."+b64))
	return "DP1." + b64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func TestCheckInstallAndExpiry(t *testing.T) {
	l := sample()
	if err := l.Check("someone-elses-install-id", t0); !errors.Is(err, ErrWrongInstall) {
		t.Errorf("wrong install: %v", err)
	}
	if !strings.Contains(l.Check("other", t0).Error(), "other") || !strings.Contains(l.Check("other", t0).Error(), l.InstallID) {
		t.Error("the wrong-install message must name both ids")
	}
	if err := l.Check(l.InstallID, l.ValidUntil.Add(-time.Second)); err != nil {
		t.Errorf("one second before expiry must pass: %v", err)
	}
	if err := l.Check(l.InstallID, l.ValidUntil); !errors.Is(err, ErrExpired) {
		t.Errorf("at expiry must be expired: %v", err)
	}
	if err := l.Check(l.InstallID, l.ValidUntil.Add(time.Hour)); !errors.Is(err, ErrExpired) {
		t.Errorf("after expiry: %v", err)
	}
	// Wrong install wins over expired: the operator's first problem is
	// that this licence is not theirs.
	if err := l.Check("other", l.ValidUntil.Add(time.Hour)); !errors.Is(err, ErrWrongInstall) {
		t.Errorf("wrong install and expired: %v", err)
	}
}

func TestStateAndDaysLeft(t *testing.T) {
	l := sample()
	if s := l.StateAt(t0); s != StateActive {
		t.Errorf("fresh: %s", s)
	}
	if s := l.StateAt(l.ValidUntil.Add(-WarnWindow + time.Hour)); s != StateExpiring {
		t.Errorf("inside the warn window: %s", s)
	}
	if s := l.StateAt(l.ValidUntil); s != StateExpired {
		t.Errorf("at expiry: %s", s)
	}
	if d := l.DaysLeft(l.ValidUntil.Add(-36 * time.Hour)); d != 1 {
		t.Errorf("36 h left = %d days, want 1 (whole days)", d)
	}
	if d := l.DaysLeft(l.ValidUntil.Add(time.Hour)); d != 0 {
		t.Errorf("after expiry = %d days, want 0", d)
	}
}

// With no vendor key compiled in, nothing verifies: a server built without
// the key accepts no licence rather than every licence.
func TestNoPublicKeyRefusesEverything(t *testing.T) {
	saved := PublicKey
	defer func() { PublicKey = saved }()
	tok := mustSign(t, sample())
	PublicKey = nil
	if _, err := Parse(tok); !errors.Is(err, ErrSignature) {
		t.Fatalf("no key: err = %v, want ErrSignature", err)
	}
}

func TestSignRefusesAnInvalidLicence(t *testing.T) {
	l := sample()
	l.Seats = -5
	if _, err := Sign(testPriv, l); err == nil {
		t.Fatal("Sign must refuse what Parse would refuse")
	}
}
