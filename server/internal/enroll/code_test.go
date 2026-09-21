package enroll

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The enrolment code (SPEC §4.8): minted by the admin, claimed once by the
// phone for a fresh credential, dead after that and after fifteen minutes.
func TestMintClaimRotateAndExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s, _ := Open(path)
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.Realm = "dialler"

	// A device created by the admin has no credential until it claims.
	id, err := s.Create("", "201", "Matt's iPhone")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "dev_") || len(id) != 10 {
		t.Fatalf("generated id: %q", id)
	}
	if ok, _ := s.Authenticate(context.Background(), id, ""); ok {
		t.Fatal("a device with no credential must not authenticate with an empty token")
	}
	if d := s.Devices()[0]; d.Label != "Matt's iPhone" || d.Enrolled || d.CodePending {
		t.Fatalf("device view: %+v", d)
	}

	code, expires, err := s.MintCode(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 8 || !expires.Equal(now.Add(CodeTTL)) {
		t.Fatalf("code %q expires %v", code, expires)
	}
	for _, c := range code {
		if !strings.ContainsRune(codeAlphabet, c) {
			t.Fatalf("code %q uses a character outside the alphabet", code)
		}
	}
	if !s.Devices()[0].CodePending {
		t.Fatal("code should be pending")
	}
	if _, _, err := s.MintCode("nobody"); err != ErrInvalid {
		t.Fatalf("minting for an unknown device: %v", err)
	}

	// Typed as a person would: lower case, a dash, an O for a 0.
	typed := strings.ToLower(code[:4]) + "-" + strings.ReplaceAll(code[4:], "0", "O")
	c, err := s.Claim(typed)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if c.DeviceID != id || c.User != "201" || !strings.HasPrefix(c.Token, "tok_") {
		t.Fatalf("claimed: %+v", c)
	}
	if ok, _ := s.Authenticate(context.Background(), id, c.Token); !ok {
		t.Fatal("the claimed token must authenticate")
	}
	if ha1, user, ok := s.DigestSecret(id); !ok || ha1 == "" || user != "201" {
		t.Fatal("the claimed token must carry its SIP Digest form")
	}
	if _, err := s.Claim(code); err != ErrBadCode {
		t.Fatalf("a code is single use: %v", err)
	}
	if d := s.Devices()[0]; !d.Enrolled || d.CodePending {
		t.Fatalf("after the claim: %+v", d)
	}

	// A new code rotates the credential: the old token dies with the claim.
	code2, _, _ := s.MintCode(id)
	c2, err := s.Claim(code2)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Authenticate(context.Background(), id, c.Token); ok {
		t.Fatal("the previous token must stop working once a new code is claimed")
	}
	if ok, _ := s.Authenticate(context.Background(), id, c2.Token); !ok {
		t.Fatal("the new token must work")
	}

	// A revoked device comes back through a claim.
	s.Revoke(id)
	if _, ok := s.UserFor(id); ok {
		t.Fatal("revoked")
	}
	code3, _, _ := s.MintCode(id)
	if _, err := s.Claim(code3); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.UserFor(id); !ok {
		t.Fatal("a claim must bring a revoked device back")
	}

	// Expiry: a code past its fifteen minutes is dead.
	code4, _, _ := s.MintCode(id)
	now = now.Add(CodeTTL + time.Second)
	if _, err := s.Claim(code4); err != ErrBadCode {
		t.Fatalf("expired code: %v", err)
	}
	if s.Devices()[0].CodePending {
		t.Fatal("an expired code is not pending")
	}

	// Persistence: the hash and expiry survive a reload, the plaintext
	// is nowhere in the file.
	code5, _, _ := s.MintCode(id)
	rawBytes, _ := os.ReadFile(path)
	raw := string(rawBytes)
	if strings.Contains(raw, code5) || strings.Contains(raw, c2.Token) {
		t.Fatal("plaintext code or token on disk")
	}
	s2, _ := Open(path)
	s2.now = func() time.Time { return now.Add(time.Minute) }
	if _, err := s2.Claim(code5); err != nil {
		t.Fatalf("claim after reload: %v", err)
	}
}

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{
		"abcd-efgh": "ABCDEFGH", "a b c d": "ABCD", "OIL0": "0110", "Zz9 ": "ZZ9",
	} {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// Another device's state is untouched by a claim, a mint or a revoke of
// this one (SPEC §4.8 isolation).
func TestCodesAreIsolatedPerDevice(t *testing.T) {
	s, _ := Open("")
	tokB, _ := s.IssueToken("dev-b", "202", "tok_fixture_for_dev_b")
	s.Create("dev-a", "201", "")
	codeA, _, _ := s.MintCode("dev-a")
	if _, err := s.Claim(codeA); err != nil {
		t.Fatal(err)
	}
	s.Revoke("dev-a")
	if ok, _ := s.Authenticate(context.Background(), "dev-b", tokB); !ok {
		t.Fatal("dev-b's credential must survive everything done to dev-a")
	}
	if d := s.Devices()[1]; d.DeviceID != "dev-b" || d.Revoked || d.CodePending {
		t.Fatalf("dev-b: %+v", d)
	}
}

func TestEnrolHandler(t *testing.T) {
	s, _ := Open("")
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	var claimed []string
	h := NewEnrolHandler(s, EnrolInfo{SignalPort: 7443, SIPDomain: "dialler", CertSHA256: "abc", Now: func() time.Time { return now }},
		Hooks{OnClaim: func(d, u string) { claimed = append(claimed, d+"/"+u) }})
	do := func(body, from string) (*http.Response, []byte) {
		req := httptest.NewRequest("POST", "/v1/enrol", strings.NewReader(body))
		req.RemoteAddr = from + ":5555"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}

	s.Create("dev-a", "201", "")
	code, _, _ := s.MintCode("dev-a")

	if resp, _ := do(`{"code":"NOPE1234"}`, "10.0.0.9"); resp.StatusCode != 404 {
		t.Fatalf("bad code: %d", resp.StatusCode)
	}
	if resp, _ := do(`{}`, "10.0.0.9"); resp.StatusCode != 400 {
		t.Fatalf("no code: %d", resp.StatusCode)
	}
	resp, body := do(`{"code":"`+code+`"}`, "10.0.0.9")
	if resp.StatusCode != 200 {
		t.Fatalf("claim: %d %s", resp.StatusCode, body)
	}
	var out ClaimResponse
	_ = json.Unmarshal(body, &out)
	if out.DeviceID != "dev-a" || out.User != "201" || !strings.HasPrefix(out.Token, "tok_") ||
		out.SignalPort != 7443 || out.SIPDomain != "dialler" || out.CertSHA256 != "abc" {
		t.Fatalf("claim response: %+v", out)
	}
	if len(claimed) != 1 || claimed[0] != "dev-a/201" {
		t.Fatalf("OnClaim: %v", claimed)
	}
	if resp, _ := do(`{"code":"`+code+`"}`, "10.0.0.9"); resp.StatusCode != 404 {
		t.Fatalf("second claim of the same code: %d", resp.StatusCode)
	}

	// Five attempts a minute per source: the sixth is refused whatever it
	// carries, another address is not, and a minute later it may try again.
	for i := 0; i < 5; i++ {
		if resp, _ := do(`{"code":"NOPE1234"}`, "10.0.0.7"); resp.StatusCode != 404 {
			t.Fatalf("attempt %d: %d", i+1, resp.StatusCode)
		}
	}
	if resp, _ := do(`{"code":"NOPE1234"}`, "10.0.0.7"); resp.StatusCode != 429 {
		t.Fatalf("sixth attempt in a minute: %d", resp.StatusCode)
	}
	if resp, _ := do(`{"code":"NOPE1234"}`, "10.0.0.8"); resp.StatusCode != 404 {
		t.Fatalf("another address is not limited: %d", resp.StatusCode)
	}
	now = now.Add(61 * time.Second)
	if resp, _ := do(`{"code":"NOPE1234"}`, "10.0.0.7"); resp.StatusCode != 404 {
		t.Fatalf("after a minute: %d", resp.StatusCode)
	}
}

func TestAdminMintsCodesAndLinks(t *testing.T) {
	s, _ := Open("")
	link := Link{Host: "10.18.0.212", HTTPSPort: 8080, CertSHA256: "c2hh"}
	h := NewAdminHandler(s, "admin", link, Hooks{})
	do := func(method, path, body string) (*http.Response, []byte) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}
	// No token given: created without a credential, code minted.
	resp, body := do("POST", "/v1/admin/devices", `{"user":"205","label":"Warehouse 3"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	id, _ := out["device_id"].(string)
	if !strings.HasPrefix(id, "dev_") || out["token"] != nil || out["label"] != "Warehouse 3" {
		t.Fatalf("create response: %v", out)
	}
	code, _ := out["code"].(string)
	if want := "dialler://enrol?c=" + code + "&f=c2hh&h=10.18.0.212&p=8080"; out["url"] != want {
		t.Fatalf("url %v, want %s", out["url"], want)
	}
	if d := s.Devices()[0]; d.Enrolled || !d.CodePending {
		t.Fatalf("device: %+v", d)
	}
	// A new code for an existing device; an unknown device is 404.
	resp, body = do("POST", "/v1/admin/devices/"+id+"/enrol-code", "")
	var cr codeResponse
	_ = json.Unmarshal(body, &cr)
	if resp.StatusCode != 200 || len(cr.Code) != 8 || cr.Code == code || !strings.Contains(cr.URL, cr.Code) {
		t.Fatalf("new code: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do("POST", "/v1/admin/devices/nope/enrol-code", ""); resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}
	// The first code is dead once a second was minted.
	if _, err := s.Claim(code); err != ErrBadCode {
		t.Fatalf("superseded code: %v", err)
	}
	if _, err := s.Claim(cr.Code); err != nil {
		t.Fatal(err)
	}
}
