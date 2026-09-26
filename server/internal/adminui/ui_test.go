package adminui

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"dialler/server/internal/admin"
	"dialler/server/internal/b2bua"
	"dialler/server/internal/diag"
	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/events"
	"dialler/server/internal/loglevel"
	"dialler/server/internal/status"
)

// fakeAPI is the call server's admin API in memory, reached through an
// http.RoundTripper so no socket is needed. It records every request.
type fakeAPI struct {
	mu       sync.Mutex
	down     bool // simulate an unreachable server
	requests []string
	devices  map[string]enroll.Device
	dirs     map[string]directory.ListResponse
	diag     map[string][]diag.Entry
	log      loglevel.View
	calls    []b2bua.CallView
	// lastIfMatch is the If-Match of the last write, for the concurrency test.
	lastIfMatch string
	lastBody    string
}

func newFakeAPI() *fakeAPI {
	t0 := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	f := &fakeAPI{devices: map[string]enroll.Device{}, dirs: map[string]directory.ListResponse{}, diag: map[string][]diag.Entry{}}
	f.devices["dev-a"] = enroll.Device{DeviceID: "dev-a", User: "201", Description: "Matt", IssuedAt: t0, UpdatedAt: t0, Enrolled: true, Config: &enroll.DeviceConfig{Version: 2, SSIDs: []string{"Office"}}, PBXLine: &enroll.PBXLine{DigestUser: "line201", Configured: true}}
	f.devices["dev-b"] = enroll.Device{DeviceID: "dev-b", User: "202", Description: "Spare", IssuedAt: t0, UpdatedAt: t0}
	f.dirs["dev-a"] = directory.ListResponse{Version: 5, Contacts: []directory.Contact{{ID: "ct_01", DisplayName: "Desk", URI: "sip:100@asterisk", Mode: directory.ModeTrunk, Favourite: true, Version: 5}}}
	f.dirs["dev-b"] = directory.ListResponse{Version: 1}
	f.diag["dev-a"] = []diag.Entry{{Name: "20260925T085512.000Z-applog.log", Kind: "applog", Size: 1234, At: "2026-09-25T08:55:12Z"}}
	f.log.Level, f.log.Startup.Level = "info", "info"
	return f
}

func (f *fakeAPI) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errors.New("dial tcp: connection refused")
	}
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
	if r.Header.Get("Authorization") != "Bearer tok" {
		return jsonResp(401, admin.ErrorBody{Error: "unauthorized"}), nil
	}
	f.lastIfMatch = strings.Trim(r.Header.Get("If-Match"), `"`)
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	f.lastBody = string(body)
	p := r.URL.Path
	seg := strings.Split(strings.TrimPrefix(p, "/v1/admin/"), "/")
	switch {
	case p == "/v1/admin/whoami":
		return jsonResp(200, map[string]bool{"ok": true}), nil
	case p == "/v1/admin/server":
		v := status.ServerView{Counts: status.Counts{Devices: len(f.devices)}}
		v.Version, v.Mode, v.LocalDomain, v.StartedAt = "test", "trunk", "dialler", time.Now().Add(-time.Hour)
		return jsonResp(200, v), nil
	case p == "/v1/admin/status":
		fv := status.FleetView{At: time.Now(), Trunk: b2bua.TrunkStatus{Configured: true, Qualify: "up"}}
		fv.Devices = append(fv.Devices, status.DeviceStatus{DeviceID: "dev-a", User: "201", Enrolled: true, Sessions: map[string]*status.Session{"app": {Online: true, Since: time.Now(), Addr: "10.0.0.5:1", AppVersion: "1.4"}}, SIP: &status.SIPStatus{Registered: true, Contact: "sip:201@10.0.0.5", ExpiresAt: time.Now().Add(time.Hour)}})
		return jsonResp(200, fv), nil
	case p == "/v1/admin/calls":
		return jsonResp(200, f.calls), nil
	case p == "/v1/admin/events":
		return jsonResp(200, EventsPage{Next: 1, Events: []events.Event{{Seq: 1, At: time.Now(), Kind: "presence", DeviceID: "dev-a", Detail: map[string]any{"online": true}}}}), nil
	case p == "/v1/admin/log" && r.Method == "GET":
		return jsonResp(200, f.log), nil
	case p == "/v1/admin/log" && r.Method == "PUT":
		var in LogRequest
		_ = json.Unmarshal(body, &in)
		if in.Level != nil && *in.Level == "debug" && in.ForSeconds == nil {
			return jsonResp(400, admin.ErrorBody{Error: "missing", Message: "for_seconds is required for debug", Field: "for_seconds"}), nil
		}
		if in.Level != nil {
			f.log.Level = *in.Level
		}
		return jsonResp(200, f.log), nil
	case p == "/v1/admin/devices" && r.Method == "GET":
		out := []enroll.Device{}
		for _, d := range f.devices {
			out = append(out, d)
		}
		return jsonResp(200, out), nil
	case p == "/v1/admin/devices" && r.Method == "POST":
		var in map[string]string
		_ = json.Unmarshal(body, &in)
		if in["user"] == "201" {
			return jsonResp(409, admin.ErrorBody{Error: "user_taken", Message: "another device already has that user", Field: "user"}), nil
		}
		f.devices["dev_NEW"] = enroll.Device{DeviceID: "dev_NEW", User: in["user"], Description: in["description"], CodePending: true}
		f.dirs["dev_NEW"] = directory.ListResponse{}
		return jsonResp(201, Created{DeviceID: "dev_NEW", User: in["user"], Description: in["description"], Code: "8XK2M4PQ", ExpiresAt: time.Now().Add(15 * time.Minute), URL: "dialler://enrol?h=10.0.0.1&p=8080&c=8XK2M4PQ"}), nil
	}
	if len(seg) >= 2 && seg[0] == "devices" {
		id := seg[1]
		d, ok := f.devices[id]
		if !ok {
			return jsonResp(404, admin.ErrorBody{Error: "not_found", Message: "device not found"}), nil
		}
		rest := strings.Join(seg[2:], "/")
		switch {
		case rest == "" && r.Method == "GET":
			return jsonResp(200, d), nil
		case rest == "" && r.Method == "PATCH":
			if f.lastIfMatch != "" && f.lastIfMatch != d.UpdatedAt.UTC().Format(time.RFC3339Nano) {
				return jsonResp(412, admin.ErrorBody{Error: "version_mismatch", Message: "changed"}), nil
			}
			var in map[string]string
			_ = json.Unmarshal(body, &in)
			d.Description = in["description"]
			d.UpdatedAt = time.Now()
			f.devices[id] = d
			return jsonResp(200, d), nil
		case rest == "" && r.Method == "DELETE":
			if r.URL.Query().Get("purge") == "1" {
				delete(f.devices, id)
			} else {
				d.Revoked = true
				f.devices[id] = d
			}
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		case rest == "enrol-code" && r.Method == "POST":
			return jsonResp(200, Code{Code: "ABCD1234", ExpiresAt: time.Now().Add(15 * time.Minute), URL: "dialler://enrol?c=ABCD1234"}), nil
		case rest == "enrol-code" && r.Method == "DELETE":
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		case rest == "config":
			var in map[string][]string
			_ = json.Unmarshal(body, &in)
			return jsonResp(200, enroll.DeviceConfig{Version: 3, SSIDs: in["ssids"]}), nil
		case rest == "pbx-line" && r.Method == "PUT":
			return jsonResp(200, enroll.PBXLine{DigestUser: "x", Configured: true}), nil
		case rest == "pbx-line" && r.Method == "DELETE":
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		case rest == "directory" && r.Method == "GET":
			return jsonResp(200, f.dirs[id]), nil
		case rest == "directory" && r.Method == "PUT":
			var in struct {
				Contacts []directory.Contact `json:"contacts"`
			}
			_ = json.Unmarshal(body, &in)
			cur := f.dirs[id]
			if r.URL.Query().Get("dry_run") == "1" {
				return jsonResp(200, directory.ReplaceResult{Version: cur.Version, Added: len(in.Contacts), Removed: len(cur.Contacts)}), nil
			}
			if f.lastIfMatch != "" && f.lastIfMatch != jsonInt(cur.Version) {
				return jsonResp(412, admin.ErrorBody{Error: "version_mismatch", Message: "changed"}), nil
			}
			cur.Version++
			cur.Contacts = in.Contacts
			f.dirs[id] = cur
			return jsonResp(200, directory.ReplaceResult{Version: cur.Version, Added: len(in.Contacts)}), nil
		case rest == "directory" && r.Method == "POST":
			var c directory.Contact
			_ = json.Unmarshal(body, &c)
			c.ID = "ct_new"
			cur := f.dirs[id]
			cur.Version++
			cur.Contacts = append(cur.Contacts, c)
			f.dirs[id] = cur
			return jsonResp(200, c), nil
		case strings.HasPrefix(rest, "directory/") && r.Method == "PUT":
			var c directory.Contact
			_ = json.Unmarshal(body, &c)
			return jsonResp(200, c), nil
		case strings.HasPrefix(rest, "directory/") && r.Method == "DELETE":
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		case rest == "diag" && r.Method == "GET":
			out := f.diag[id]
			if out == nil {
				out = []diag.Entry{}
			}
			return jsonResp(200, out), nil
		case strings.HasPrefix(rest, "diag/") && r.Method == "GET":
			h := http.Header{}
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Content-Disposition", `attachment; filename="x.log"`)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("log line\n")), Header: h}, nil
		case strings.HasPrefix(rest, "diag") && r.Method == "DELETE":
			return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		}
	}
	return jsonResp(404, admin.ErrorBody{Error: "not_found", Message: "no route " + r.Method + " " + p}), nil
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }

func jsonResp(code int, v any) *http.Response {
	b, _ := json.Marshal(v)
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(b)), Header: h}
}

// harness is a UI over the fake API with a logged-in browser.
type harness struct {
	t      *testing.T
	api    *fakeAPI
	ui     *UI
	cookie *http.Cookie
	csrf   string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	api := newFakeAPI()
	ui, err := New(Config{
		Client:   newTestClient(api, "tok"),
		Sessions: NewSessions("pw"),
		QR: func(link string) (template.HTML, error) {
			return template.HTML("<svg data-link='" + template.HTMLEscapeString(link) + "'></svg>"), nil
		},
		Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, api: api, ui: ui}
	// Log in.
	rec := h.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"pw"}}, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			h.cookie = c
		}
	}
	if h.cookie == nil {
		t.Fatal("no session cookie after login")
	}
	h.csrf, _ = ui.cfg.Sessions.Check(h.cookie.Value)
	return h
}

func (h *harness) do(method, path string, form url.Values, mutate func(*http.Request)) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		if h != nil && h.csrf != "" && form.Get("csrf") == "" {
			form.Set("csrf", h.csrf)
		}
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if h.cookie != nil {
		req.AddCookie(h.cookie)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ui.ServeHTTP(rec, req)
	return rec
}

func (h *harness) get(path string) *httptest.ResponseRecorder { return h.do("GET", path, nil, nil) }

func (h *harness) post(path string, kv ...string) *httptest.ResponseRecorder {
	form := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		form.Add(kv[i], kv[i+1])
	}
	return h.do("POST", path, form, nil)
}

func mustContain(t *testing.T, rec *httptest.ResponseRecorder, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(rec.Body.String(), w) {
			t.Fatalf("response (%d) lacks %q:\n%s", rec.Code, w, rec.Body.String()[:min(len(rec.Body.String()), 3000)])
		}
	}
}

func TestLoginRequiredAndWrongPassword(t *testing.T) {
	api := newFakeAPI()
	ui, _ := New(Config{Client: newTestClient(api, "tok"), Sessions: NewSessions("pw")})
	h := &harness{t: t, ui: ui, api: api}
	if rec := h.get("/clients"); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
		t.Fatalf("no session: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	rec := h.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"nope"}}, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not right") {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	rec = h.do("POST", "/login", url.Values{"username": {"root"}, "password": {"pw"}}, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not right") {
		t.Fatalf("wrong username: %d", rec.Code)
	}
	if rec := h.get("/login"); !strings.Contains(rec.Body.String(), `name="username"`) {
		t.Fatal("the login page must ask for a username")
	}
	if len(api.requests) != 0 {
		t.Fatalf("the API was called before login: %v", api.requests)
	}
}

func TestClientsPage(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/clients")
	if rec.Code != 200 {
		t.Fatalf("fleet: %d", rec.Code)
	}
	mustContain(t, rec, "<h1>Clients</h1>", "dev-a", "Matt", "201", "202", "trunk up", "Add a client", `name="csrf" value="`+h.csrf+`"`)
	if strings.Contains(rec.Body.String(), "Fleet") || strings.Contains(rec.Body.String(), "Devices") {
		t.Fatal("the page is named Clients")
	}
}

func TestCreateClientShowsCodeAndQR(t *testing.T) {
	h := newHarness(t)
	rec := h.post("/clients", "user", "204", "description", "Warehouse 3")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/clients/dev_NEW/code?k=") {
		t.Fatalf("create: %d %s %s", rec.Code, rec.Header().Get("Location"), rec.Body)
	}
	rec = h.get(rec.Header().Get("Location"))
	if rec.Code != 200 {
		t.Fatalf("code page: %d", rec.Code)
	}
	mustContain(t, rec, "8XK2M4PQ", "data-link='dialler://enrol?h=10.0.0.1&amp;p=8080&amp;c=8XK2M4PQ'", "Ext 204")
	// The code is shown once: a reload has nothing to show and offers a new one.
	rec = h.get("/clients/dev_NEW/code")
	mustContain(t, rec, "Make an enrolment code")
	if strings.Contains(rec.Body.String(), "8XK2M4PQ") {
		t.Fatal("the code was shown twice")
	}
	// Minting from the client page goes to the code page with the new code.
	rec = h.post("/clients/dev-a/code")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mint: %d", rec.Code)
	}
	mustContain(t, h.get(rec.Header().Get("Location")), "ABCD1234")
	// A taken user: the add page comes back with the error and the form kept.
	rec = h.post("/clients", "user", "201", "description", "Dup")
	mustContain(t, rec, "another device already has that user", `value="Dup"`)
	// The add page itself.
	mustContain(t, h.get("/clients/new"), "<h1>Add a client</h1>")
}

func TestClientPageAndActions(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/clients/dev-a")
	if rec.Code != 200 {
		t.Fatalf("device: %d", rec.Code)
	}
	mustContain(t, rec, "Matt", "Office", "line201", "Desk", "sip:100@asterisk", "applog.log", "Purge", "<h2>Status</h2>", "<h2>Enrolment</h2>", "<h2>Settings</h2>")
	if rec := h.get("/clients/nope"); rec.Code != 404 {
		t.Fatalf("unknown device: %d", rec.Code)
	}
	// Description with the right If-Match.
	rec = h.post("/clients/dev-a/description", "description", "Renamed", "updated_at", h.api.devices["dev-a"].UpdatedAt.UTC().Format(time.RFC3339Nano))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body)
	}
	if h.api.devices["dev-a"].Description != "Renamed" {
		t.Fatal("rename did not reach the API")
	}
	// Stale If-Match → 412 explained, nothing changed.
	rec = h.post("/clients/dev-a/description", "description", "Again", "updated_at", "2020-01-01T00:00:00Z")
	mustContain(t, rec, "Changed by someone else since you opened this")
	if h.api.devices["dev-a"].Description != "Renamed" {
		t.Fatal("a refused rename changed the record")
	}
	// Purge needs the id typed.
	rec = h.post("/clients/dev-a/purge", "confirm", "wrong")
	mustContain(t, rec, "type the client id")
	if _, ok := h.api.devices["dev-a"]; !ok {
		t.Fatal("purged without confirmation")
	}
	rec = h.post("/clients/dev-a/purge", "confirm", "dev-a")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/clients?flash=") {
		t.Fatalf("purge: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if _, ok := h.api.devices["dev-a"]; ok {
		t.Fatal("purge did not reach the API")
	}
	// Config: SSIDs one per line, pushed with the version.
	rec = h.post("/clients/dev-b/config", "ssids", "Office\n Office-5G \n", "config_version", "0")
	if rec.Code != http.StatusSeeOther || !strings.Contains(h.api.lastBody, `["Office","Office-5G"]`) || h.api.lastIfMatch != "0" {
		t.Fatalf("config: %d body %s if-match %q", rec.Code, h.api.lastBody, h.api.lastIfMatch)
	}
	// The flash survives the redirect.
	rec = h.get(rec.Header().Get("Location"))
	mustContain(t, rec, "saved and pushed")
}

func TestCSRFAndSessionExpiryReplay(t *testing.T) {
	h := newHarness(t)
	// Wrong CSRF token: refused, API untouched.
	before := len(h.api.requests)
	rec := h.do("POST", "/clients/dev-a/revoke", url.Values{"csrf": {"bogus"}}, nil)
	if rec.Code != http.StatusForbidden || len(h.api.requests) != before {
		t.Fatalf("bad csrf: %d, api calls %d→%d", rec.Code, before, len(h.api.requests))
	}
	// Session expired mid-form: the form is stashed, login is shown, and
	// the form is applied after login.
	h.ui.cfg.Sessions.Logout(h.cookie.Value)
	rec = h.do("POST", "/clients/dev-a/description", url.Values{"description": {"After expiry"}, "updated_at": {h.api.devices["dev-a"].UpdatedAt.UTC().Format(time.RFC3339Nano)}}, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "session had expired") {
		t.Fatalf("expired POST: %d", rec.Code)
	}
	resume := ""
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if i := strings.Index(line, `name="resume" value="`); i >= 0 {
			resume = strings.SplitN(line[i+len(`name="resume" value="`):], `"`, 2)[0]
		}
	}
	if resume == "" {
		t.Fatal("no resume key in the login page")
	}
	h.cookie = nil
	rec = h.do("POST", "/login", url.Values{"username": {"admin"}, "password": {"pw"}, "resume": {resume}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login with resume should replay the form: %d %s", rec.Code, rec.Body)
	}
	if h.api.devices["dev-a"].Description != "After expiry" {
		t.Fatalf("the stashed form was not applied: %q", h.api.devices["dev-a"].Description)
	}
}

func TestUnreachableServerBannerAndLastData(t *testing.T) {
	h := newHarness(t)
	if rec := h.get("/clients"); rec.Code != 200 {
		t.Fatal("warm the cache")
	}
	h.api.down = true
	rec := h.get("/clients")
	if rec.Code != 200 {
		t.Fatalf("fleet while down: %d", rec.Code)
	}
	mustContain(t, rec, "not answering", "last read at", "dev-a", "disabled")
	if !strings.Contains(rec.Body.String(), `aria-disabled="true"`) {
		t.Fatal("writes must be disabled while the server is down")
	}
	rec = h.post("/clients/dev-a/revoke")
	mustContain(t, rec, "not answering", "nothing was changed")
	// A page never fetched shows the banner with no data.
	rec = h.get("/calls")
	mustContain(t, rec, "not answering")
	h.api.down = false
	if rec := h.get("/clients"); strings.Contains(rec.Body.String(), "not answering") {
		t.Fatal("banner persisted after the server came back")
	}
}

func TestDirectoryCSVUploadPreviewThenApply(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/clients/dev-a/directory.csv")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("download: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Body.String() != "display_name,uri,mode,favourite\nDesk,sip:100@asterisk,trunk,true\n" {
		t.Fatalf("csv: %q", rec.Body.String())
	}
	// Upload: multipart with a file; bare numbers get the local domain.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("csv", "")
	fw, _ := mw.CreateFormFile("file", "dir.csv")
	_, _ = fw.Write([]byte("display_name,uri,mode\nAlice,201,local\nDesk,sip:100@asterisk,trunk\n"))
	mw.Close()
	req := httptest.NewRequest("POST", "/clients/dev-a/directory/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(h.cookie)
	q := req.URL.Query()
	req.URL.RawQuery = q.Encode()
	// CSRF rides as a form field: add it to the multipart body instead.
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	_ = mw.WriteField("csrf", h.csrf)
	fw, _ = mw.CreateFormFile("file", "dir.csv")
	_, _ = fw.Write([]byte("display_name,uri,mode\nAlice,201,local\nDesk,sip:100@asterisk,trunk\n"))
	mw.Close()
	req = httptest.NewRequest("POST", "/clients/dev-a/directory/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(h.cookie)
	rec = httptest.NewRecorder()
	h.ui.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	mustContain(t, rec, "Replace the directory?", "<strong>2</strong> added", "<strong>1</strong> removed", `name="stage" value="confirm"`, `name="directory_version" value="5"`)
	if !strings.Contains(h.api.requests[len(h.api.requests)-2], "dry_run=1") {
		t.Fatalf("preview must be a dry run: %v", h.api.requests)
	}
	if h.api.dirs["dev-a"].Version != 5 {
		t.Fatal("the preview applied something")
	}
	// Confirm applies against the version the preview saw.
	rec = h.post("/clients/dev-a/directory/upload", "stage", "confirm", "directory_version", "5", "csv", "display_name,uri,mode\nAlice,201,local\n")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if h.api.lastIfMatch != "5" || !strings.Contains(h.api.lastBody, `"uri":"sip:201@dialler"`) {
		t.Fatalf("apply: if-match %q body %s", h.api.lastIfMatch, h.api.lastBody)
	}
	// A stale confirm is refused and explained.
	rec = h.post("/clients/dev-a/directory/upload", "stage", "confirm", "directory_version", "5", "csv", "display_name,uri,mode\nBob,202,local\n")
	mustContain(t, rec, "Changed by someone else")
}

func TestCopyDirectoryReportsPerDevice(t *testing.T) {
	h := newHarness(t)
	rec := h.do("POST", "/clients/dev-a/directory/copy", url.Values{"targets": {"dev-b", "nope"}}, nil)
	if rec.Code != 200 {
		t.Fatalf("copy: %d %s", rec.Code, rec.Body)
	}
	mustContain(t, rec, "Copy directory: results", "202", "1 added", "failed: Not found")
	if len(h.api.dirs["dev-b"].Contacts) != 1 {
		t.Fatal("dev-b did not receive the copy")
	}
}

func TestServerAndDiagnosticsPages(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/server")
	if rec.Code != 200 {
		t.Fatalf("server: %d", rec.Code)
	}
	mustContain(t, rec, "<h1>Server</h1>", "Identity", "<h2>Clients</h2>")
	if strings.Contains(rec.Body.String(), "Fleet") || strings.Contains(rec.Body.String(), "Recent events") {
		t.Fatal("the server page must not say Fleet nor carry the events (those are Diagnostics)")
	}
	rec = h.get("/diagnostics")
	if rec.Code != 200 {
		t.Fatalf("diagnostics: %d", rec.Code)
	}
	mustContain(t, rec, "<h1>Diagnostics</h1>", "Server logging", "Recent events", "presence", ">Matt<", "Files uploaded")
	rec = h.post("/diagnostics/log", "level", "debug")
	mustContain(t, rec, "for_seconds is required")
	rec = h.post("/diagnostics/log", "level", "debug", "for_seconds", "600")
	if rec.Code != http.StatusSeeOther || h.api.log.Level != "debug" {
		t.Fatalf("set log: %d level %s", rec.Code, h.api.log.Level)
	}
	rec = h.post("/diagnostics/log", "level", "info", "for_seconds", "x")
	mustContain(t, rec, "whole number")
	// The nav order is Server, Clients, Calls, Diagnostics.
	body := h.get("/clients").Body.String()
	i := strings.Index(body, ">Server</a>")
	j := strings.Index(body, ">Clients</a>")
	k := strings.Index(body, ">Calls</a>")
	l := strings.Index(body, ">Diagnostics</a>")
	if !(i > 0 && i < j && j < k && k < l) {
		t.Fatalf("nav order: %d %d %d %d", i, j, k, l)
	}
}

func TestCallsPageAndDiagDownload(t *testing.T) {
	h := newHarness(t)
	rec := h.get("/calls")
	mustContain(t, rec, "No calls in progress")
	h.api.calls = []b2bua.CallView{{CallID: "c1", State: "bridged", Since: time.Now(), A: &b2bua.LegView{Leg: "app", User: "201"}, B: &b2bua.LegView{Leg: "trunk"}}}
	rec = h.get("/calls")
	mustContain(t, rec, "bridged", "trunk")
	rec = h.get("/clients/dev-a/diag/20260925T085512.000Z-applog.log")
	if rec.Code != 200 || rec.Body.String() != "log line\n" || rec.Header().Get("Content-Disposition") == "" {
		t.Fatalf("diag download: %d %q", rec.Code, rec.Body.String())
	}
}

func TestCSVRoundTrip(t *testing.T) {
	in := []directory.Contact{{DisplayName: `Quote "Q"`, URI: "sip:1@x", Mode: directory.ModeLocal, Favourite: true}, {DisplayName: "Comma, Inc", URI: "sip:2@x", Mode: directory.ModeTrunk}}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadCSV(strings.NewReader("\xef\xbb\xbf" + buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].DisplayName != in[0].DisplayName || !out[0].Favourite || out[1].DisplayName != in[1].DisplayName || out[1].Favourite {
		t.Fatalf("%+v", out)
	}
	for _, bad := range []string{"", "name,uri\nA,1\n", "display_name,uri,mode\nA,,local\n", "display_name,uri,mode\nA,1,nope\n", "display_name,uri,mode,favourite\nA,1,local,maybe\n"} {
		if _, err := ReadCSV(strings.NewReader(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// Columns in any order, favourite optional, blank lines skipped.
	out, err = ReadCSV(strings.NewReader("mode,display_name,uri\nlocal,A,1\n\ntrunk,B,sip:2@x\n"))
	if err != nil || len(out) != 2 || out[1].Mode != directory.ModeTrunk {
		t.Fatalf("%v %+v", err, out)
	}
}
