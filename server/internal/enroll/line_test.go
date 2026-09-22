package enroll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dialler/server/internal/secrets"
)

func testStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	box, err := secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Secrets = box
	return s
}

// The device's line on the PBX (SPEC §6 item 3c): set per device, sealed at
// rest, and readable back only by the registrar.
func TestSetPBXLinePersistsAndIsolates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s := testStore(t, path)
	s.Create("dev-a", "201", "")
	s.Create("dev-b", "202", "")

	if _, ok := s.PBXLine("dev-a"); ok {
		t.Fatal("no line until an admin sets one")
	}
	if d := s.Devices()[0]; d.PBXLine != nil {
		t.Fatalf("device view: %+v", d)
	}

	line, err := s.SetPBXLine("dev-a", "", " matt ", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if line.DigestUser != "matt" || !line.Configured || line.DN != "" {
		t.Fatalf("set: %+v (an empty DN means the device's own user)", line)
	}

	cred, ok, err := s.PBXCredential("dev-a")
	if err != nil || !ok {
		t.Fatalf("credential: %v %v", ok, err)
	}
	if cred.DN != "201" || cred.DigestUser != "matt" || cred.Secret != "hunter2" || cred.User != "201" {
		t.Fatalf("credential = %+v; DN should fall back to the device's user", cred)
	}

	// An explicit DN wins, for a cluster whose numbering is not ours.
	if _, err := s.SetPBXLine("dev-b", "7002", "jo", "s2"); err != nil {
		t.Fatal(err)
	}
	cred, _, _ = s.PBXCredential("dev-b")
	if cred.DN != "7002" {
		t.Fatalf("explicit DN ignored: %+v", cred)
	}

	// Both are required: half a credential could never register.
	if _, err := s.SetPBXLine("dev-a", "", "", "s"); err == nil {
		t.Error("a line with no digest user should be refused")
	}
	if _, err := s.SetPBXLine("dev-a", "", "matt", ""); err == nil {
		t.Error("a line with no secret should be refused")
	}
	if _, err := s.SetPBXLine("nobody", "", "matt", "s"); err != ErrInvalid {
		t.Errorf("unknown device: %v", err)
	}

	// Across a restart, since a fleet must not need re-provisioning after a
	// bounce.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Secrets = s.Secrets
	cred, ok, err = s2.PBXCredential("dev-a")
	if err != nil || !ok || cred.Secret != "hunter2" {
		t.Fatalf("after reload: %+v %v %v", cred, ok, err)
	}
}

func TestTheSecretIsNeverStoredInClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s := testStore(t, path)
	s.Create("dev-a", "201", "")
	if _, err := s.SetPBXLine("dev-a", "", "matt", "hunter2"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hunter2") {
		t.Fatalf("the credential is on disk in clear:\n%s", raw)
	}
	// Nor in the device view the admin API lists.
	b, _ := json.Marshal(s.Devices())
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("the credential reached the device listing: %s", b)
	}
}

func TestAStoreWithNoKeyRefusesToHoldACredential(t *testing.T) {
	s, _ := Open("")
	s.Create("dev-a", "201", "")
	if _, err := s.SetPBXLine("dev-a", "", "matt", "hunter2"); err != ErrNoSecretKey {
		t.Fatalf("want ErrNoSecretKey, got %v — a secret must never fall back to plaintext", err)
	}
}

func TestPBXCredentialsSkipsRevokedDevices(t *testing.T) {
	s := testStore(t, "")
	for _, d := range []struct{ id, user string }{{"dev-a", "201"}, {"dev-b", "202"}, {"dev-c", "203"}} {
		s.Create(d.id, d.user, "")
		if _, err := s.SetPBXLine(d.id, "", "u-"+d.user, "s"); err != nil {
			t.Fatal(err)
		}
	}
	// A revoked device's phone cannot connect, so keeping its line
	// registered would have the exchange ring a number nobody can answer.
	if _, err := s.Revoke("dev-b"); err != nil {
		t.Fatal(err)
	}
	creds, err := s.PBXCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 2 || creds[0].DeviceID != "dev-a" || creds[1].DeviceID != "dev-c" {
		t.Fatalf("credentials = %+v", creds)
	}
}

func TestOneUnreadableLineDoesNotHideTheRest(t *testing.T) {
	s := testStore(t, "")
	s.Create("dev-a", "201", "")
	s.Create("dev-b", "202", "")
	if _, err := s.SetPBXLine("dev-a", "", "a", "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPBXLine("dev-b", "", "b", "s"); err != nil {
		t.Fatal(err)
	}
	// Corrupt one record, as a hand-edited or half-restored file would.
	s.mu.Lock()
	s.devices["dev-b"].PBXLine.SecretEnc = "v1:AAAA"
	s.mu.Unlock()

	creds, err := s.PBXCredentials()
	if err == nil {
		t.Fatal("the unreadable line should be reported")
	}
	if !strings.Contains(err.Error(), "dev-b") {
		t.Errorf("the error should name the device, got %v", err)
	}
	if len(creds) != 1 || creds[0].DeviceID != "dev-a" {
		t.Fatalf("the healthy line should still be returned, got %+v", creds)
	}
}

func TestDeletePBXLine(t *testing.T) {
	s := testStore(t, "")
	s.Create("dev-a", "201", "")
	if _, err := s.SetPBXLine("dev-a", "", "matt", "s"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.DeletePBXLine("dev-a"); !ok || err != nil {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if _, ok := s.PBXLine("dev-a"); ok {
		t.Error("the line is still there")
	}
	// Deleting twice is not an error; deleting a device that is not there
	// is a miss.
	if ok, err := s.DeletePBXLine("dev-a"); !ok || err != nil {
		t.Errorf("second delete: %v %v", ok, err)
	}
	if ok, _ := s.DeletePBXLine("nobody"); ok {
		t.Error("unknown device should report a miss")
	}
}

func TestAdminPBXLineRoutes(t *testing.T) {
	s := testStore(t, "")
	s.Create("dev-a", "201", "")
	var pushed []*PBXCredential
	h := NewAdminHandler(s, "admin", Link{}, Hooks{OnPBXLine: func(d string, l *PBXCredential) {
		if d != "dev-a" {
			t.Fatalf("pushed for %s", d)
		}
		pushed = append(pushed, l)
	}})
	do := func(method, path, body string) (*http.Response, []byte) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}

	if resp, _ := do("GET", "/v1/admin/devices/dev-a/pbx-line", ""); resp.StatusCode != 404 {
		t.Fatalf("unset line: %d", resp.StatusCode)
	}
	if resp, _ := do("PUT", "/v1/admin/devices/dev-zz/pbx-line", `{"digest_user":"m","secret":"s"}`); resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}
	if resp, _ := do("PUT", "/v1/admin/devices/dev-a/pbx-line", `{"digest_user":"m"}`); resp.StatusCode != 400 {
		t.Fatalf("no secret: %d", resp.StatusCode)
	}

	resp, body := do("PUT", "/v1/admin/devices/dev-a/pbx-line", `{"digest_user":"matt","secret":"hunter2","dn":"7001"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("set: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "hunter2") {
		t.Fatalf("the secret came back in the reply: %s", body)
	}
	var line PBXLine
	_ = json.Unmarshal(body, &line)
	if line.DN != "7001" || line.DigestUser != "matt" || !line.Configured {
		t.Fatalf("set reply: %+v", line)
	}

	resp, body = do("GET", "/v1/admin/devices/dev-a/pbx-line", "")
	if resp.StatusCode != 200 || strings.Contains(string(body), "hunter2") {
		t.Fatalf("get: %d %s — no read may return the secret", resp.StatusCode, body)
	}

	// busybox wget, the harness's in-network helper, has no PUT.
	if resp, _ := do("POST", "/v1/admin/devices/dev-a/pbx-line", `{"digest_user":"matt","secret":"s2"}`); resp.StatusCode != 200 {
		t.Fatalf("POST alias: %d", resp.StatusCode)
	}

	if resp, _ := do("DELETE", "/v1/admin/devices/dev-a/pbx-line", ""); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	if len(pushed) != 3 || pushed[0] == nil || pushed[0].Secret != "hunter2" || pushed[1].Secret != "s2" || pushed[2] != nil {
		t.Fatalf("OnPBXLine: %+v (two sets then a removal)", pushed)
	}

	// Unauthenticated, like every other admin verb.
	req := httptest.NewRequest("GET", "/v1/admin/devices/dev-a/pbx-line", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("no bearer: %d", rec.Code)
	}
}
