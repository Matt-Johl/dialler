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
)

func TestIssueAuthenticateRevokePersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.Issue("dev1", "201")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Authenticate(context.Background(), "dev1", tok); !ok {
		t.Fatal("fresh token should authenticate")
	}
	if ok, _ := s.Authenticate(context.Background(), "dev1", tok+"x"); ok {
		t.Fatal("wrong token accepted")
	}
	if u, ok := s.UserFor("dev1"); !ok || u != "201" {
		t.Fatalf("UserFor = %q,%v", u, ok)
	}

	// Re-issue invalidates the old token.
	tok2, _ := s.Issue("dev1", "201")
	if ok, _ := s.Authenticate(context.Background(), "dev1", tok); ok {
		t.Fatal("old token still valid after re-issue")
	}

	// Persisted: a fresh Open sees the new token.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s2.Authenticate(context.Background(), "dev1", tok2); !ok {
		t.Fatal("token not persisted")
	}

	if ok, _ := s2.Revoke("dev1"); !ok {
		t.Fatal("revoke unknown")
	}
	if ok, _ := s2.Authenticate(context.Background(), "dev1", tok2); ok {
		t.Fatal("revoked token accepted")
	}
	if _, ok := s2.UserFor("dev1"); ok {
		t.Fatal("revoked device still maps to a user")
	}
	if _, err := s.Issue("", "x"); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}

	// Fixed tokens (dev fixtures) are honoured, re-issuable, and length-checked.
	s3, _ := Open("")
	got, err := s3.IssueToken("dev-a", "201", "tok_dev_a_fixed_0001")
	if err != nil || got != "tok_dev_a_fixed_0001" {
		t.Fatalf("fixed token: %q %v", got, err)
	}
	if ok, _ := s3.Authenticate(context.Background(), "dev-a", "tok_dev_a_fixed_0001"); !ok {
		t.Fatal("fixed token should authenticate")
	}
	if _, err := s3.IssueToken("dev-a", "201", "short"); err != ErrInvalid {
		t.Fatalf("short fixed token accepted: %v", err)
	}
}

func TestAdminHandler(t *testing.T) {
	s, _ := Open("")
	var issued []string
	h := NewAdminHandler(s, "admin-secret", Hooks{OnIssue: func(d, u string) { issued = append(issued, d+"/"+u) }})

	// Handlers are exercised via ServeHTTP + a recorder: no sockets needed.
	do := func(method, path, body, bearer string) *http.Response {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result()
	}

	if resp := do("POST", "/v1/admin/devices", `{"device_id":"d","user":"1"}`, "wrong"); resp.StatusCode != 401 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	resp := do("POST", "/v1/admin/devices", `{"device_id":"dev1","user":"201"}`, "admin-secret")
	if resp.StatusCode != 201 {
		t.Fatalf("issue: %d", resp.StatusCode)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.HasPrefix(out["token"], "tok_") {
		t.Fatalf("no token in %v", out)
	}
	if len(issued) != 1 || issued[0] != "dev1/201" {
		t.Fatalf("hook not called: %v", issued)
	}

	// Device auth middleware accepts the issued token.
	protected := s.DeviceAuth(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	viaDevice := func(token string) int {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Device-ID", "dev1")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		protected.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := viaDevice(out["token"]); code != 204 {
		t.Fatalf("device auth: %d", code)
	}
	if code := viaDevice("nope"); code != 401 {
		t.Fatalf("device auth with bad token: %d", code)
	}

	if resp := do("POST", "/v1/admin/devices", `{"device_id":"","user":"1"}`, "admin-secret"); resp.StatusCode != 400 {
		t.Fatalf("invalid enrolment: %d", resp.StatusCode)
	}
	// A store that cannot persist is a server fault, not a client one.
	broken, _ := Open(t.TempDir() + "/not-a-dir/x/devices.json")
	_ = os.WriteFile(filepath.Dir(filepath.Dir(broken.path)), []byte("file, not dir"), 0o600)
	brokenH := NewAdminHandler(broken, "admin-secret", Hooks{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/admin/devices", strings.NewReader(`{"device_id":"d","user":"1"}`))
	req.Header.Set("Authorization", "Bearer admin-secret")
	brokenH.ServeHTTP(rec, req)
	if rec.Code != 500 {
		t.Fatalf("persist failure should be 500, got %d: %s", rec.Code, rec.Body.String())
	}

	if resp := do("GET", "/v1/admin/devices", "", "admin-secret"); resp.StatusCode != 200 {
		t.Fatalf("list: %d", resp.StatusCode)
	}
	if resp := do("DELETE", "/v1/admin/devices/dev1", "", "admin-secret"); resp.StatusCode != 204 {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}
	if resp := do("DELETE", "/v1/admin/devices/ghost", "", "admin-secret"); resp.StatusCode != 404 {
		t.Fatalf("revoke unknown: %d", resp.StatusCode)
	}
}
