package enroll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"dialler/server/internal/admin"
	"dialler/server/internal/secrets"
)

func adminDo(h http.Handler, method, path, body string) (*httptest.ResponseRecorder, admin.ErrorBody) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var e admin.ErrorBody
	if rec.Code >= 400 {
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
	}
	return rec, e
}

// Every error the admin enrolment routes answer is the §4.4 envelope with
// a stable code, and a bad body never reaches the store.
func TestAdminErrorsAreTheEnvelope(t *testing.T) {
	s, _ := Open("")
	h := NewAdminHandler(s, "admin", Link{}, Hooks{})

	req := httptest.NewRequest("GET", "/v1/admin/devices", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var e admin.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != 401 || e.Error != admin.CodeUnauthorized || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("no token: %d %s", rec.Code, rec.Body)
	}

	cases := []struct {
		name, method, path, body string
		status                   int
		code, field              string
	}{
		{"missing user", "POST", "/v1/admin/devices", `{"description":"x"}`, 400, admin.CodeMissing, "user"},
		{"bad user", "POST", "/v1/admin/devices", `{"user":"2 01"}`, 400, admin.CodeInvalid, "user"},
		{"bad device id", "POST", "/v1/admin/devices", `{"user":"201","device_id":"../x"}`, 400, admin.CodeInvalid, "device_id"},
		{"unknown field", "POST", "/v1/admin/devices", `{"user":"201","nonsense":true}`, 400, admin.CodeUnknownField, "nonsense"},
		{"duplicate key", "POST", "/v1/admin/devices", `{"user":"201","user":"202"}`, 400, admin.CodeBadJSON, ""},
		{"wrong type", "POST", "/v1/admin/devices", `{"user":201}`, 400, admin.CodeInvalid, "user"},
		{"short token", "POST", "/v1/admin/devices", `{"user":"201","token":"short"}`, 400, admin.CodeInvalid, "token"},
		{"control char in description", "POST", "/v1/admin/devices", "{\"user\":\"201\",\"description\":\"a\\u0000b\"}", 400, admin.CodeInvalid, "description"},
		{"not json", "POST", "/v1/admin/devices", `user=201`, 400, admin.CodeBadJSON, ""},
		{"revoke unknown", "DELETE", "/v1/admin/devices/nope", "", 404, admin.CodeNotFound, ""},
		// A ".." segment never reaches a handler: the mux cleans the path
		// and answers 301, so the grammar is exercised with an id that is
		// merely wrong.
		{"revoke bad id", "DELETE", "/v1/admin/devices/-x", "", 404, admin.CodeNotFound, ""},
		{"code for unknown", "POST", "/v1/admin/devices/nope/enrol-code", "", 404, admin.CodeNotFound, ""},
		{"config unknown device", "GET", "/v1/admin/devices/nope/config", "", 404, admin.CodeNotFound, ""},
		{"config missing ssids", "PUT", "/v1/admin/devices/dev-a/config", `{}`, 400, admin.CodeMissing, "ssids"},
		{"config bad ssid", "PUT", "/v1/admin/devices/dev-a/config", "{\"ssids\":[\"ok\",\"bad\\u0007\"]}", 400, admin.CodeInvalid, "ssids[1]"},
		{"config wrong type", "PUT", "/v1/admin/devices/dev-a/config", `{"ssids":"Office"}`, 400, admin.CodeInvalid, "ssids"},
		{"line missing secret", "PUT", "/v1/admin/devices/dev-a/pbx-line", `{"digest_user":"u"}`, 400, admin.CodeMissing, "secret"},
		{"line bad dn", "PUT", "/v1/admin/devices/dev-a/pbx-line", `{"digest_user":"u","secret":"s","dn":"abc"}`, 400, admin.CodeInvalid, "dn"},
		{"line unknown device", "PUT", "/v1/admin/devices/nope/pbx-line", `{"digest_user":"u","secret":"s"}`, 404, admin.CodeNotFound, ""},
		{"line delete unknown", "DELETE", "/v1/admin/devices/nope/pbx-line", "", 404, admin.CodeNotFound, ""},
	}
	s.Create("dev-a", "201", "")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec, e := adminDo(h, c.method, c.path, c.body)
			if rec.Code != c.status || e.Error != c.code {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, c.status, c.code)
			}
			if c.field != "" && e.Field != c.field {
				t.Fatalf("field %q, want %q (%s)", e.Field, c.field, e.Message)
			}
		})
	}
	// None of the bad bodies created a device or touched dev-a.
	devs := s.Devices()
	if len(devs) != 1 || devs[0].DeviceID != "dev-a" || devs[0].Config != nil || devs[0].PBXLine != nil {
		t.Fatalf("a refused request mutated the store: %+v", devs)
	}
}

// Revoked is an authentication state: every admin route still works on
// the record, and re-POSTing an id is 409 without a token, a credential
// replace with one (ADMIN-API.md §5.1).
func TestAdminRevokedAndRePost(t *testing.T) {
	s, _ := Open("")
	s.Secrets, _ = secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	var pushed int
	h := NewAdminHandler(s, "admin", Link{}, Hooks{OnConfig: func(string, DeviceConfig) { pushed++ }})
	if rec, _ := adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-a","user":"201","description":"A"}`); rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec, e := adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-a","user":"201"}`); rec.Code != 409 || e.Error != admin.CodeDeviceExists {
		t.Fatalf("re-post without token: %d %s", rec.Code, rec.Body)
	}
	if rec, e := adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-b","user":"201"}`); rec.Code != 409 || e.Error != admin.CodeUserTaken {
		t.Fatalf("taken user: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := adminDo(h, "PUT", "/v1/admin/devices/dev-a/config", `{"ssids":["Office"]}`); rec.Code != 200 || pushed != 1 {
		t.Fatalf("config: %d pushed %d", rec.Code, pushed)
	}
	if rec, _ := adminDo(h, "PUT", "/v1/admin/devices/dev-a/config", `{"ssids":["Office"]}`); rec.Code != 200 || pushed != 1 {
		t.Fatalf("identical config must not push: %d pushed %d", rec.Code, pushed)
	}
	if rec, _ := adminDo(h, "PUT", "/v1/admin/devices/dev-a/pbx-line", `{"digest_user":"u","secret":"s"}`); rec.Code != 200 {
		t.Fatalf("line: %d %s", rec.Code, rec.Body)
	}
	if rec, _ := adminDo(h, "DELETE", "/v1/admin/devices/dev-a", ""); rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	for _, p := range []string{"/v1/admin/devices/dev-a/config", "/v1/admin/devices/dev-a/pbx-line"} {
		if rec, _ := adminDo(h, "GET", p, ""); rec.Code != 200 {
			t.Fatalf("GET %s on a revoked device: %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec, _ := adminDo(h, "PUT", "/v1/admin/devices/dev-a/pbx-line", `{"digest_user":"u2","secret":"s2"}`); rec.Code != 200 {
		t.Fatalf("line on a revoked device: %d %s", rec.Code, rec.Body)
	}
	// The harness re-provisions with a fixed token: credential only.
	rec, _ := adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-a","user":"201","token":"tok_fixed_0123456789"}`)
	if rec.Code != 201 {
		t.Fatalf("re-post with token: %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["token"] != "tok_fixed_0123456789" || out["description"] != "A" {
		t.Fatalf("re-post reply: %v", out)
	}
	d, _ := s.Device("dev-a")
	if d.Description != "A" || d.Config == nil || d.PBXLine == nil || d.PBXLine.DigestUser != "u2" || !d.Revoked {
		t.Fatalf("re-post disturbed the record: %+v", d)
	}
	if rec, e := adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-a","user":"999","token":"tok_fixed_0123456789"}`); rec.Code != 409 || e.Error != admin.CodeImmutable {
		t.Fatalf("user change: %d %s", rec.Code, rec.Body)
	}
}

// The step-4 routes: one device, rename with If-Match, purge, cancel code.
func TestAdminDeviceRoutes(t *testing.T) {
	s, _ := Open("")
	var purged, revoked []string
	h := NewAdminHandler(s, "admin", Link{}, Hooks{
		OnPurge:  func(id string) { purged = append(purged, id) },
		OnRevoke: func(id string) { revoked = append(revoked, id) },
	})
	adminDo(h, "POST", "/v1/admin/devices", `{"device_id":"dev-a","user":"201","description":"A"}`)

	rec, _ := adminDo(h, "GET", "/v1/admin/devices/dev-a", "")
	if rec.Code != 200 {
		t.Fatalf("get one: %d %s", rec.Code, rec.Body)
	}
	var d Device
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	if d.DeviceID != "dev-a" || d.Description != "A" || !d.CodePending || d.CodeExpiresAt.IsZero() || d.UpdatedAt.IsZero() {
		t.Fatalf("device view: %+v", d)
	}
	if rec, _ := adminDo(h, "GET", "/v1/admin/devices/nope", ""); rec.Code != 404 {
		t.Fatalf("get unknown: %d", rec.Code)
	}

	// PATCH with the right If-Match, then with a stale one.
	req := httptest.NewRequest("PATCH", "/v1/admin/devices/dev-a", strings.NewReader(`{"description":"B"}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("If-Match", `"`+d.UpdatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")+`"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("patch with matching If-Match: %d %s", rec.Code, rec.Body)
	}
	var d2 Device
	_ = json.Unmarshal(rec.Body.Bytes(), &d2)
	if d2.Description != "B" || !d2.UpdatedAt.After(d.UpdatedAt) {
		t.Fatalf("after patch: %+v", d2)
	}
	req = httptest.NewRequest("PATCH", "/v1/admin/devices/dev-a", strings.NewReader(`{"description":"C"}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("If-Match", `"`+d.UpdatedAt.Format("2006-01-02T15:04:05.999999999Z07:00")+`"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 412 || errorBodyOf(t, rec).Error != admin.CodeVersionMismatch {
		t.Fatalf("patch with stale If-Match: %d %s", rec.Code, rec.Body)
	}
	if got, _ := s.Device("dev-a"); got.Description != "B" {
		t.Fatal("a refused patch changed the record")
	}
	req = httptest.NewRequest("PATCH", "/v1/admin/devices/dev-a", strings.NewReader(`{"description":"C"}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("If-Match", `"yesterday"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("malformed If-Match: %d", rec.Code)
	}
	if rec, e := adminDo(h, "PATCH", "/v1/admin/devices/dev-a", `{}`); rec.Code != 400 || e.Field != "description" {
		t.Fatalf("patch without description: %d %s", rec.Code, rec.Body)
	}
	if rec, e := adminDo(h, "PATCH", "/v1/admin/devices/dev-a", `{"user":"9"}`); rec.Code != 400 || e.Error != admin.CodeUnknownField {
		t.Fatalf("patch user: %d %s", rec.Code, rec.Body)
	}

	// Config If-Match is the settings' version.
	adminDo(h, "PUT", "/v1/admin/devices/dev-a/config", `{"ssids":["Office"]}`)
	req = httptest.NewRequest("PUT", "/v1/admin/devices/dev-a/config", strings.NewReader(`{"ssids":["Other"]}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("If-Match", `"0"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 412 {
		t.Fatalf("config with stale version: %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest("PUT", "/v1/admin/devices/dev-a/config", strings.NewReader(`{"ssids":["Other"]}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("If-Match", `"1"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("config with current version: %d %s", rec.Code, rec.Body)
	}

	// Cancel the code, then purge.
	if rec, _ := adminDo(h, "DELETE", "/v1/admin/devices/dev-a/enrol-code", ""); rec.Code != 204 {
		t.Fatalf("cancel code: %d", rec.Code)
	}
	if got, _ := s.Device("dev-a"); got.CodePending {
		t.Fatal("code still pending after cancel")
	}
	if rec, _ := adminDo(h, "DELETE", "/v1/admin/devices/dev-a/enrol-code", ""); rec.Code != 204 {
		t.Fatalf("cancel with none pending: %d", rec.Code)
	}
	if rec, _ := adminDo(h, "DELETE", "/v1/admin/devices/nope/enrol-code", ""); rec.Code != 404 {
		t.Fatalf("cancel unknown: %d", rec.Code)
	}
	if rec, _ := adminDo(h, "DELETE", "/v1/admin/devices/dev-a?purge=maybe", ""); rec.Code != 400 {
		t.Fatalf("bad purge value: %d", rec.Code)
	}
	if rec, _ := adminDo(h, "DELETE", "/v1/admin/devices/dev-a?purge=1", ""); rec.Code != 204 {
		t.Fatalf("purge: %d %s", rec.Code, rec.Body)
	}
	if s.Exists("dev-a") || len(purged) != 1 || purged[0] != "dev-a" || len(revoked) != 0 {
		t.Fatalf("after purge: exists=%v purged=%v revoked=%v", s.Exists("dev-a"), purged, revoked)
	}
	if rec, _ := adminDo(h, "DELETE", "/v1/admin/devices/dev-a?purge=1", ""); rec.Code != 404 {
		t.Fatalf("purge again: %d", rec.Code)
	}
	if rec, _ := adminDo(h, "GET", "/v1/admin/devices/dev-a", ""); rec.Code != 404 {
		t.Fatalf("get after purge: %d", rec.Code)
	}
}

func errorBodyOf(t *testing.T, rec *httptest.ResponseRecorder) admin.ErrorBody {
	t.Helper()
	var e admin.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e
}

func TestAdminBodyLimitsAndContentType(t *testing.T) {
	s, _ := Open("")
	h := NewAdminHandler(s, "admin", Link{}, Hooks{})
	big := `{"user":"201","description":"` + strings.Repeat("x", 5000) + `"}`
	rec, e := adminDo(h, "POST", "/v1/admin/devices", big)
	if rec.Code != 413 || e.Error != admin.CodeTooLarge {
		t.Fatalf("over the limit: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("POST", "/v1/admin/devices", strings.NewReader(`{"user":"201"}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "text/plain")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Fatalf("text/plain body: %d", rec.Code)
	}
}

// Fuzz every write route with arbitrary bodies: no panic, only the
// statuses the contract names, and the store still opens and holds the
// same devices afterwards (ADMIN-API.md rule 6; SPEC 9b).
func FuzzAdminWrites(f *testing.F) {
	seeds := []string{`{"user":"201"}`, `{"user":"201","device_id":"dev-x","token":"tok_1234567890123456"}`,
		`{"ssids":["Office"]}`, `{"digest_user":"u","secret":"s","dn":"201"}`, `{`, `[]`, "\xff", `{"user":"201","user":"1"}`,
		`{"ssids":[` + strings.Repeat(`"a",`, 100) + `"a"]}`}
	for _, s := range seeds {
		for route := 0; route < 3; route++ {
			f.Add(route, []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, route int, body []byte) {
		path := filepath.Join(t.TempDir(), "devices.json")
		s, err := Open(path)
		if err != nil {
			t.Skip()
		}
		s.Secrets, _ = secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
		s.Create("dev-a", "201", "")
		h := NewAdminHandler(s, "admin", Link{}, Hooks{})
		var method, p string
		switch ((route % 3) + 3) % 3 {
		case 0:
			method, p = "POST", "/v1/admin/devices"
		case 1:
			method, p = "PUT", "/v1/admin/devices/dev-a/config"
		case 2:
			method, p = "PUT", "/v1/admin/devices/dev-a/pbx-line"
		}
		req := httptest.NewRequest(method, p, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case 200, 201, 204, 400, 404, 409, 413, 415:
		default:
			t.Fatalf("status %d for %q", rec.Code, body)
		}
		if rec.Code >= 400 {
			var e admin.ErrorBody
			if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error == "" {
				t.Fatalf("error without envelope: %d %s", rec.Code, rec.Body)
			}
		}
		// Whatever happened, the file re-opens and says the same thing.
		before := s.Devices()
		again, err := Open(path)
		if err != nil {
			t.Fatalf("store unreadable after %q: %v", body, err)
		}
		after := again.Devices()
		if len(before) != len(after) {
			t.Fatalf("round trip lost devices: %d → %d", len(before), len(after))
		}
		for i := range before {
			if before[i].DeviceID != after[i].DeviceID || before[i].User != after[i].User {
				t.Fatalf("round trip changed a device: %+v → %+v", before[i], after[i])
			}
		}
	})
}
