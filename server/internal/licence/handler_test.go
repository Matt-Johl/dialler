package licence

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dialler/server/internal/admin"
)

type errBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Field   string `json:"field"`
}

func licenceDo(h http.Handler, method, body string, auth bool) (*httptest.ResponseRecorder, errBody) {
	req := httptest.NewRequest(method, "/v1/admin/licence", strings.NewReader(body))
	if auth {
		req.Header.Set("Authorization", "Bearer admin")
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var e errBody
	if rec.Code >= 400 {
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
	}
	return rec, e
}

func newTestHandler(t *testing.T) (http.Handler, *Manager, string) {
	t.Helper()
	dir := t.TempDir()
	m := Open(filepath.Join(dir, "licence"), sample().InstallID, func() time.Time { return t0 }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	used := 2
	return NewHandler(m, "admin", func() int { return used }), m, dir
}

func body(tok string) string {
	b, _ := json.Marshal(map[string]string{"licence": tok})
	return string(b)
}

func TestLicenceRoutesGuardAndMethods(t *testing.T) {
	h, _, _ := newTestHandler(t)
	if rec, e := licenceDo(h, "GET", "", false); rec.Code != 401 || e.Error != admin.CodeUnauthorized {
		t.Fatalf("no bearer: %d %+v", rec.Code, e)
	}
	if rec, _ := licenceDo(h, "DELETE", "", true); rec.Code != 405 {
		t.Fatalf("DELETE: %d", rec.Code)
	}
	rec, _ := licenceDo(h, "GET", "", true)
	if rec.Code != 200 {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	var s Summary
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if s.State != StateMissing || s.InstallID != sample().InstallID || s.SeatsUsed != 2 || s.Seats != 0 {
		t.Fatalf("GET with nothing installed: %+v", s)
	}
}

// Every refusal of a PUT is the §4.4 envelope with a reason the operator
// can act on, and none of them changes what is installed.
func TestLicencePutRefusals(t *testing.T) {
	h, m, dir := newTestHandler(t)
	good := mustSign(t, sample())
	if rec, e := licenceDo(h, "PUT", body(good), true); rec.Code != 200 {
		t.Fatalf("install the good licence first: %d %+v", rec.Code, e)
	}

	tok := good
	parts := strings.SplitN(tok, ".", 3)
	wrong := sample()
	wrong.InstallID = "ffffffffffffffffffffffffffffffff"
	expired := sample()
	expired.IssuedAt, expired.ValidUntil = t0.Add(-48*time.Hour), t0.Add(-time.Hour)
	cases := []struct {
		name, body  string
		status      int
		code, inMsg string
	}{
		{"not json", `licence`, 400, admin.CodeBadJSON, ""},
		{"missing field", `{}`, 400, admin.CodeMissing, "licence"},
		{"unknown field", `{"licence":"x","seats":9}`, 400, admin.CodeUnknownField, ""},
		{"over 4 KiB", body("DL1." + strings.Repeat("A", 5000) + ".sig"), 413, admin.CodeTooLarge, ""},
		{"bad prefix", body("DL2." + parts[1] + "." + parts[2]), 400, admin.CodeInvalid, "not a licence token"},
		{"two parts", body("DL1." + parts[1]), 400, admin.CodeInvalid, "payload and signature"},
		{"four parts", body(tok + ".extra"), 400, admin.CodeInvalid, "payload and signature"},
		{"bad base64", body("DL1.!!!." + parts[2]), 400, admin.CodeInvalid, "base64"},
		{"edited payload", body("DL1." + parts[1][:len(parts[1])-2] + "AA." + parts[2]), 400, admin.CodeInvalid, "signature"},
		{"wrong install", body(mustSign(t, wrong)), 400, admin.CodeInvalid, "ffffffffffffffffffffffffffffffff"},
		{"expired", body(mustSign(t, expired)), 400, admin.CodeInvalid, "expired"},
		{"unknown payload field", body(signRaw(`{"v":1,"id":"a","customer":"c","install_id":"i","seats":1,"issued_at":"2026-10-03T12:00:00Z","valid_until":"2027-10-03T12:00:00Z","x":1}`)), 400, admin.CodeInvalid, "payload"},
	}
	for _, c := range cases {
		rec, e := licenceDo(h, "PUT", c.body, true)
		if rec.Code != c.status || e.Error != c.code || !strings.Contains(e.Message, c.inMsg) {
			t.Errorf("%s: got %d %+v, want %d %s containing %q", c.name, rec.Code, e, c.status, c.code, c.inMsg)
		}
	}
	// The wrong-install message names both ids.
	_, e := licenceDo(h, "PUT", body(mustSign(t, wrong)), true)
	if !strings.Contains(e.Message, sample().InstallID) || e.Field != "licence" {
		t.Errorf("wrong install message/field: %+v", e)
	}
	// Nothing above disturbed the installed licence, in memory or on disk.
	if m.Token() != good || m.Seats() != 10 {
		t.Fatalf("a refusal changed the licence: seats=%d", m.Seats())
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "licence"))
	if strings.TrimSpace(string(raw)) != good {
		t.Fatal("a refusal rewrote the file")
	}
}

func TestLicencePutInstallsAndGetReflectsIt(t *testing.T) {
	h, m, dir := newTestHandler(t)
	tok := mustSign(t, sample())
	rec, _ := licenceDo(h, "POST", body(tok), true) // POST is accepted for PUT
	if rec.Code != 200 {
		t.Fatalf("POST: %d %s", rec.Code, rec.Body)
	}
	var s Summary
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if s.State != StateActive || s.Seats != 10 || s.SeatsUsed != 2 || s.Customer != "Example Ltd" || s.DaysLeft != 365 {
		t.Fatalf("summary after install: %+v", s)
	}
	st, err := os.Stat(filepath.Join(dir, "licence"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("licence file: %v %v", err, st.Mode())
	}
	rec, _ = licenceDo(h, "GET", "", true)
	_ = json.Unmarshal(rec.Body.Bytes(), &s)
	if s.State != StateActive || s.ID != "lic_TESTX001" {
		t.Fatalf("GET after install: %+v", s)
	}
	// And a fresh manager on the same directory reads it back.
	again := Open(filepath.Join(dir, "licence"), sample().InstallID, func() time.Time { return t0 }, nil)
	if again.Token() != m.Token() {
		t.Fatal("the installed licence did not survive a reopen")
	}
}

func TestLicencePutWriteFailureKeepsThePrevious(t *testing.T) {
	h, m, dir := newTestHandler(t)
	good := mustSign(t, sample())
	licenceDo(h, "PUT", body(good), true)
	if os.Getuid() == 0 {
		t.Skip("root ignores permissions")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o700)
	l := sample()
	l.Seats, l.ID = 3, "lic_TESTX009"
	rec, e := licenceDo(h, "PUT", body(mustSign(t, l)), true)
	if rec.Code != 500 || e.Error != admin.CodeStore {
		t.Fatalf("write failure: %d %+v", rec.Code, e)
	}
	if m.Seats() != 10 || m.Token() != good {
		t.Fatal("a failed write changed the licence in force")
	}
}

// Concurrent installs never hang and leave exactly one licence, which is
// one of the two offered.
func TestLicenceConcurrentPutsSettle(t *testing.T) {
	h, m, _ := newTestHandler(t)
	a := mustSign(t, sample())
	l := sample()
	l.ID, l.Seats = "lic_TESTX002", 7
	b := mustSign(t, l)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok := a
			if i%2 == 1 {
				tok = b
			}
			if rec, _ := licenceDo(h, "PUT", body(tok), true); rec.Code != 200 {
				t.Errorf("concurrent PUT %d: %d", i, rec.Code)
			}
		}(i)
	}
	wg.Wait()
	if tok := m.Token(); tok != a && tok != b {
		t.Fatalf("installed token is neither: %q", tok)
	}
	if s := m.Seats(); s != 10 && s != 7 {
		t.Fatalf("seats %d", s)
	}
}
