package directory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dialler/server/internal/admin"
)

func adminDo(h http.Handler, method, path, body string) (*httptest.ResponseRecorder, admin.ErrorBody) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var e admin.ErrorBody
	if rec.Code >= 400 {
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
	}
	return rec, e
}

func TestAdminDirectoryErrorsAreTheEnvelope(t *testing.T) {
	s, _ := Open("")
	known := func(id string) bool { return id == "dev-a" }
	h := NewAdminHandler(s, "admin", known)
	c, _ := s.Upsert("dev-a", Contact{DisplayName: "A", URI: "sip:1@x", Mode: ModeLocal})

	cases := []struct {
		name, method, path, body string
		status                   int
		code, field              string
	}{
		{"unknown device", "GET", "/v1/admin/devices/nope/directory", "", 404, admin.CodeNotFound, ""},
		{"bad device id", "GET", "/v1/admin/devices/-x/directory", "", 404, admin.CodeNotFound, ""},
		{"replace missing contacts", "PUT", "/v1/admin/devices/dev-a/directory", `{"version":1}`, 400, admin.CodeMissing, "contacts"},
		{"replace unknown field", "PUT", "/v1/admin/devices/dev-a/directory", `{"contacts":[],"extra":1}`, 400, admin.CodeUnknownField, "extra"},
		{"replace bad contact", "PUT", "/v1/admin/devices/dev-a/directory", `{"contacts":[{"display_name":"A","uri":"sip:1@x","mode":"local"},{"display_name":"B","uri":"sip:2@x","mode":"nope"}]}`, 400, admin.CodeInvalid, "contacts[1].mode"},
		{"replace missing uri", "PUT", "/v1/admin/devices/dev-a/directory", `{"contacts":[{"display_name":"A","mode":"local"}]}`, 400, admin.CodeMissing, "contacts[0].uri"},
		{"replace duplicate uri", "PUT", "/v1/admin/devices/dev-a/directory", `{"contacts":[{"uri":"sip:1@x","mode":"local"},{"uri":"SIP:1@x","mode":"local"}]}`, 400, admin.CodeInvalid, "contacts[1].uri"},
		{"replace wrong type", "PUT", "/v1/admin/devices/dev-a/directory", `{"contacts":{}}`, 400, admin.CodeInvalid, "contacts"},
		{"upsert control char", "POST", "/v1/admin/devices/dev-a/directory", "{\"display_name\":\"a\\u0000\",\"uri\":\"sip:3@x\",\"mode\":\"local\"}", 400, admin.CodeInvalid, "display_name"},
		{"upsert space in uri", "POST", "/v1/admin/devices/dev-a/directory", `{"uri":"sip:3 @x","mode":"local"}`, 400, admin.CodeInvalid, "uri"},
		{"edit bad cid", "PUT", "/v1/admin/devices/dev-a/directory/x_1", `{"uri":"sip:1@x","mode":"local"}`, 404, admin.CodeNotFound, ""},
		{"delete bad cid", "DELETE", "/v1/admin/devices/dev-a/directory/nope", "", 404, admin.CodeNotFound, ""},
		{"delete unknown cid", "DELETE", "/v1/admin/devices/dev-a/directory/ct_00", "", 404, admin.CodeNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, e := adminDo(h, tc.method, tc.path, tc.body)
			if rec.Code != tc.status || e.Error != tc.code {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, tc.status, tc.code)
			}
			if tc.field != "" && e.Field != tc.field {
				t.Fatalf("field %q, want %q (%s)", e.Field, tc.field, e.Message)
			}
		})
	}
	contacts, version := s.Contacts("dev-a")
	if version != 1 || len(contacts) != 1 || contacts[0].ID != c.ID {
		t.Fatalf("a refused request changed the directory: v%d %+v", version, contacts)
	}
	// A GET body sent straight back as a PUT is accepted: version is a
	// declared field, and an identical list is a no-op.
	rec, _ := adminDo(h, "PUT", "/v1/admin/devices/dev-a/directory", `{"version":1,"contacts":[{"id":"`+c.ID+`","display_name":"A","uri":"sip:1@x","mode":"local","version":1,"updated_at":"2026-01-01T00:00:00Z"}]}`)
	if rec.Code != 200 {
		t.Fatalf("round-tripped list: %d %s", rec.Code, rec.Body)
	}
	var res ReplaceResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Version != 1 || res.Added+res.Changed+res.Removed != 0 {
		t.Fatalf("identical replace was not a no-op: %+v", res)
	}
}

// Step 5: the dry run counts without applying, and If-Match on every
// write is the directory's version.
func TestAdminDirectoryDryRunAndIfMatch(t *testing.T) {
	s, _ := Open("")
	var notified int
	s.OnChange(func(string, int64) { notified++ })
	h := NewAdminHandler(s, "admin", func(string) bool { return true })
	adminDo(h, "POST", "/v1/admin/devices/dev-a/directory", `{"display_name":"A","uri":"sip:1@x","mode":"local"}`)
	adminDo(h, "POST", "/v1/admin/devices/dev-a/directory", `{"display_name":"B","uri":"sip:2@x","mode":"local"}`)
	if notified != 2 {
		t.Fatalf("notified %d", notified)
	}
	upload := `{"contacts":[{"display_name":"A2","uri":"sip:1@x","mode":"local"},{"display_name":"C","uri":"sip:3@x","mode":"trunk"}]}`
	rec, _ := adminDo(h, "PUT", "/v1/admin/devices/dev-a/directory?dry_run=1", upload)
	if rec.Code != 200 {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	var res ReplaceResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Version != 2 || res.Added != 1 || res.Changed != 1 || res.Removed != 1 {
		t.Fatalf("dry run counts: %+v", res)
	}
	if contacts, v := s.Contacts("dev-a"); v != 2 || len(contacts) != 2 || notified != 2 {
		t.Fatalf("dry run applied something: v%d %d contacts, notified %d", v, len(contacts), notified)
	}
	if rec, _ := adminDo(h, "PUT", "/v1/admin/devices/dev-a/directory?dry_run=yes", upload); rec.Code != 400 {
		t.Fatalf("bad dry_run value: %d", rec.Code)
	}
	// A stale If-Match is refused with nothing changed; the current one applies.
	put := func(ifMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/v1/admin/devices/dev-a/directory", strings.NewReader(upload))
		req.Header.Set("Authorization", "Bearer admin")
		req.Header.Set("If-Match", ifMatch)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := put(`"1"`); rec.Code != 412 || errorBodyOf(t, rec).Error != admin.CodeVersionMismatch {
		t.Fatalf("stale If-Match: %d %s", rec.Code, rec.Body)
	}
	if _, v := s.Contacts("dev-a"); v != 2 || notified != 2 {
		t.Fatal("a refused replace changed something")
	}
	if rec := put(`"2"`); rec.Code != 200 {
		t.Fatalf("current If-Match: %d %s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Version != 3 || res.Added != 1 || res.Changed != 1 || res.Removed != 1 || notified != 3 {
		t.Fatalf("applied: %+v notified %d", res, notified)
	}
	// Per-contact writes take the same header.
	contacts, _ := s.Contacts("dev-a")
	req := httptest.NewRequest("DELETE", "/v1/admin/devices/dev-a/directory/"+contacts[0].ID, nil)
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("If-Match", `"2"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 412 {
		t.Fatalf("delete with stale If-Match: %d", rec.Code)
	}
	req = httptest.NewRequest("POST", "/v1/admin/devices/dev-a/directory", strings.NewReader(`{"uri":"sip:9@x","mode":"local"}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("If-Match", `"3"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("add with current If-Match: %d %s", rec.Code, rec.Body)
	}
}

func errorBodyOf(t *testing.T, rec *httptest.ResponseRecorder) admin.ErrorBody {
	t.Helper()
	var e admin.ErrorBody
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e
}

func TestAdminDirectoryReplaceLimit(t *testing.T) {
	s, _ := Open("")
	h := NewAdminHandler(s, "admin", func(string) bool { return true })
	var b strings.Builder
	b.WriteString(`{"contacts":[`)
	for i := 0; i <= maxContacts; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"uri":"sip:` + strings.Repeat("9", 5) + string(rune('a'+i%26)) + `@x","mode":"local"}`)
	}
	b.WriteString(`]}`)
	rec, e := adminDo(h, "PUT", "/v1/admin/devices/dev-a/directory", b.String())
	if rec.Code != 400 || e.Field != "contacts" {
		t.Fatalf("%d contacts: %d %s", maxContacts+1, rec.Code, rec.Body)
	}
}

// Fuzz the two admin write shapes: no panic, only contract statuses, an
// envelope on every error, and the device's file still round-trips.
func FuzzAdminDirectoryWrites(f *testing.F) {
	seeds := []string{`{"contacts":[{"uri":"sip:1@x","mode":"local"}]}`, `{"uri":"sip:1@x","mode":"trunk","favourite":true}`,
		`{"contacts":[]}`, `{`, `[]`, "\xff", `{"contacts":[{"uri":"sip:1@x","uri":"sip:2@x","mode":"local"}]}`}
	for _, s := range seeds {
		f.Add(true, []byte(s))
		f.Add(false, []byte(s))
	}
	f.Fuzz(func(t *testing.T, replace bool, body []byte) {
		dir := t.TempDir()
		s, err := Open(dir)
		if err != nil {
			t.Skip()
		}
		h := NewAdminHandler(s, "admin", func(string) bool { return true })
		method, p := "POST", "/v1/admin/devices/dev-a/directory"
		if replace {
			method = "PUT"
		}
		req := httptest.NewRequest(method, p, strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		switch rec.Code {
		case 200, 400, 404, 413, 415:
		default:
			t.Fatalf("status %d for %q", rec.Code, body)
		}
		if rec.Code >= 400 {
			var e admin.ErrorBody
			if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error == "" {
				t.Fatalf("error without envelope: %d %s", rec.Code, rec.Body)
			}
		}
		before, v1 := s.Contacts("dev-a")
		again, err := Open(dir)
		if err != nil {
			t.Fatalf("store unreadable after %q: %v", body, err)
		}
		after, v2 := again.Contacts("dev-a")
		if v1 != v2 || len(before) != len(after) {
			t.Fatalf("round trip differs: v%d/%d contacts %d/%d", v1, v2, len(before), len(after))
		}
	})
}
