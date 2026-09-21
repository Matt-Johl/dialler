package adminui

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/pbxconfig"
	"dialler/server/internal/status"
	"dialler/server/internal/wire"
)

// fakeAPI stands in for the call server's admin API: the routes the UI
// uses, over in-memory state, with the real packages' JSON shapes. It is
// served through a RoundTripper, so no socket is opened.
type fakeAPI struct {
	mu       sync.Mutex
	token    string
	devices  []status.Device
	dirs     map[string][]directory.Contact
	configs  map[string]enroll.DeviceConfig
	replaced map[string]int // ReplaceDirectory calls per device
	revoked  []string
	purged   []string
	pbx      *pbxconfig.Settings
	pbxCreds map[string]enroll.PBXCredentials
	minted   int
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		token: "secret",
		devices: []status.Device{
			{Device: enroll.Device{DeviceID: "dev-a", User: "201", Label: "Matt's iPhone", Enrolled: true}, Online: []wire.ClientKind{wire.ClientApp, wire.ClientExtension}, Registered: true},
			{Device: enroll.Device{DeviceID: "dev-b", User: "202", Enrolled: false, CodePending: true}, Online: []wire.ClientKind{}},
		},
		dirs: map[string][]directory.Contact{
			"dev-a": {
				{ID: "ct_1", DisplayName: "Desk", URI: "sip:100@asterisk", Mode: directory.ModeTrunk, Version: 1},
				{ID: "ct_2", DisplayName: "Matt", URI: "sip:201@dialler", Mode: directory.ModeLocal, Favourite: true, Version: 2},
			},
		},
		configs:  map[string]enroll.DeviceConfig{},
		replaced: map[string]int{},
		pbxCreds: map[string]enroll.PBXCredentials{},
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "unauthorized", 401)
		return
	}
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	p := strings.TrimPrefix(r.URL.Path, "/v1/admin/")
	parts := strings.Split(p, "/")
	switch {
	case r.Method == "GET" && p == "status":
		devs := make([]status.Device, len(f.devices))
		copy(devs, f.devices)
		for i := range devs {
			if c, ok := f.pbxCreds[devs[i].DeviceID]; ok {
				devs[i].PBX = &enroll.PBXIdentity{User: c.User, DeviceName: c.DeviceName}
				devs[i].PBXState = "pending"
			}
		}
		write(status.Response{Server: status.Server{PublicHost: "10.0.0.1", SIPDomain: "dialler", SignalPort: 7443, SIPPort: 5061, HTTPSPort: 8080, CertSHA256: "fp", Trunk: "sip:asterisk:5060;transport=udp"}, PBX: f.pbx, Devices: devs, Now: time.Now()})
	case r.Method == "GET" && p == "pbx":
		if f.pbx == nil {
			http.Error(w, "none", 404)
			return
		}
		write(f.pbx)
	case r.Method == "PUT" && p == "pbx":
		var in pbxconfig.Settings
		_ = json.NewDecoder(r.Body).Decode(&in)
		if err := in.Validate(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		in.Version = 1
		f.pbx = &in
		write(in)
	case len(parts) == 3 && parts[2] == "pbx" && r.Method == "PUT":
		var in enroll.PBXCredentials
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Password == "" {
			if old, ok := f.pbxCreds[parts[1]]; ok {
				in.Password = old.Password
			} else {
				http.Error(w, "password required", 400)
				return
			}
		}
		f.pbxCreds[parts[1]] = in
		write(enroll.PBXIdentity{User: in.User, DeviceName: in.DeviceName})
	case len(parts) == 3 && parts[2] == "pbx" && r.Method == "DELETE":
		delete(f.pbxCreds, parts[1])
		w.WriteHeader(204)
	case r.Method == "POST" && p == "devices":
		var in struct {
			DeviceID string `json:"device_id"`
			User     string `json:"user"`
			Label    string `json:"label"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.User == "" {
			http.Error(w, "enroll: device_id and user are required", 400)
			return
		}
		if in.DeviceID != "" && !enroll.ValidDeviceID(in.DeviceID) {
			http.Error(w, enroll.ErrBadDeviceID.Error(), 400)
			return
		}
		id := in.DeviceID
		if id == "" {
			id = "dev_NEW001"
		}
		f.devices = append(f.devices, status.Device{Device: enroll.Device{DeviceID: id, User: in.User, Label: in.Label, CodePending: true}, Online: []wire.ClientKind{}})
		f.minted++
		w.WriteHeader(201)
		write(map[string]any{"device_id": id, "user": in.User, "label": in.Label, "code": "A7K2M9PX", "expires_at": time.Now().Add(15 * time.Minute), "url": "dialler://enrol?c=A7K2M9PX&f=fp&h=10.0.0.1&p=8080"})
	case len(parts) == 2 && parts[0] == "devices" && r.Method == "PUT":
		var in struct{ User, Label string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		for i := range f.devices {
			if f.devices[i].DeviceID == parts[1] {
				f.devices[i].User, f.devices[i].Label = in.User, in.Label
				write(f.devices[i].Device)
				return
			}
		}
		http.NotFound(w, r)
	case len(parts) == 2 && parts[0] == "devices" && r.Method == "DELETE":
		if r.URL.Query().Get("purge") == "1" {
			f.purged = append(f.purged, parts[1])
			kept := f.devices[:0]
			for _, d := range f.devices {
				if d.DeviceID != parts[1] {
					kept = append(kept, d)
				}
			}
			f.devices = kept
		} else {
			f.revoked = append(f.revoked, parts[1])
		}
		w.WriteHeader(204)
	case len(parts) == 3 && parts[2] == "enrol-code" && r.Method == "POST":
		if !f.known(parts[1]) {
			http.NotFound(w, r)
			return
		}
		f.minted++
		write(map[string]any{"code": "BFF6GB4Z", "expires_at": time.Now().Add(15 * time.Minute), "url": "dialler://enrol?c=BFF6GB4Z&f=fp&h=10.0.0.1&p=8080"})
	case len(parts) == 3 && parts[2] == "directory" && r.Method == "GET":
		write(directory.ListResponse{Version: 2, Contacts: f.dirs[parts[1]]})
	case len(parts) == 3 && parts[2] == "directory" && r.Method == "PUT":
		var in directory.ListResponse
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.replaced[parts[1]]++
		f.dirs[parts[1]] = in.Contacts
		write(directory.ReplaceResult{Version: 3, Added: len(in.Contacts)})
	case len(parts) == 3 && parts[2] == "directory" && r.Method == "POST":
		var c directory.Contact
		_ = json.NewDecoder(r.Body).Decode(&c)
		c.ID = "ct_new"
		f.dirs[parts[1]] = append(f.dirs[parts[1]], c)
		write(c)
	case len(parts) == 4 && parts[2] == "directory" && r.Method == "PUT":
		var c directory.Contact
		_ = json.NewDecoder(r.Body).Decode(&c)
		for i, old := range f.dirs[parts[1]] {
			if old.ID == parts[3] {
				c.ID = parts[3]
				f.dirs[parts[1]][i] = c
			}
		}
		write(c)
	case len(parts) == 4 && parts[2] == "directory" && r.Method == "DELETE":
		kept := f.dirs[parts[1]][:0]
		for _, c := range f.dirs[parts[1]] {
			if c.ID != parts[3] {
				kept = append(kept, c)
			}
		}
		f.dirs[parts[1]] = kept
		w.WriteHeader(204)
	case len(parts) == 3 && parts[2] == "config" && r.Method == "GET":
		if c, ok := f.configs[parts[1]]; ok {
			write(c)
		} else {
			http.Error(w, "no settings", 404)
		}
	case len(parts) == 3 && parts[2] == "config" && r.Method == "PUT":
		var in struct{ SSIDs []string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		c := enroll.DeviceConfig{Version: int64(f.configs[parts[1]].Version + 1), SSIDs: in.SSIDs}
		f.configs[parts[1]] = c
		write(c)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) known(id string) bool {
	for _, d := range f.devices {
		if d.DeviceID == id {
			return true
		}
	}
	return false
}

// handlerTransport routes the client's requests straight into a handler.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// browser drives the app through ServeHTTP, keeping the session cookie
// and the CSRF token as a browser would.
type browser struct {
	t      *testing.T
	app    *App
	cookie *http.Cookie
	csrf   string
}

func (b *browser) do(method, path string, form url.Values, body io.Reader, contentType string) *httptest.ResponseRecorder {
	b.t.Helper()
	var req *http.Request
	if form != nil {
		if b.csrf != "" {
			form.Set("_csrf", b.csrf)
		}
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, body)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}
	req.RemoteAddr = "10.0.0.9:1234"
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	rec := httptest.NewRecorder()
	b.app.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if c.MaxAge < 0 {
				b.cookie = nil
			} else {
				b.cookie = c
			}
		}
	}
	if b.cookie != nil && b.csrf == "" {
		if sess := b.app.sessions.get(b.cookie.Value); sess != nil {
			b.csrf = b.app.sessions.csrf(sess)
		}
	}
	return rec
}

func (b *browser) get(path string) *httptest.ResponseRecorder { return b.do("GET", path, nil, nil, "") }
func (b *browser) post(path string, form url.Values) *httptest.ResponseRecorder {
	return b.do("POST", path, form, nil, "")
}

func setup(t *testing.T) (*App, *fakeAPI, *browser) {
	t.Helper()
	api := newFakeAPI()
	base, _ := url.Parse("https://dialler:8080")
	hash, err := HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(Config{
		Client:       NewClient(base, "secret", handlerTransport{api}),
		PasswordHash: hash,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, api, &browser{t: t, app: app}
}

func signIn(t *testing.T, b *browser) {
	t.Helper()
	rec := b.post("/login", url.Values{"password": {"correct horse"}, "next": {"/"}})
	if rec.Code != 303 || b.cookie == nil {
		t.Fatalf("sign in: %d cookie=%v", rec.Code, b.cookie)
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := HashPassword("correct horse")
	if err != nil || !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Fatalf("%q %v", h, err)
	}
	if !VerifyPassword(h, "correct horse") || VerifyPassword(h, "Correct horse") || VerifyPassword("garbage", "x") {
		t.Fatal("verify")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("a short password must be refused")
	}
}

func TestLoginGatesEverything(t *testing.T) {
	_, _, b := setup(t)
	rec := b.get("/devices/dev-a")
	if rec.Code != 303 || !strings.HasPrefix(rec.Header().Get("Location"), "/login?next=") {
		t.Fatalf("signed out: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := b.get("/static/app.css"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet must not need a session: %d", rec.Code)
	}
	if rec := b.post("/login", url.Values{"password": {"wrong"}}); rec.Code != 401 || !strings.Contains(rec.Body.String(), "not right") {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	for i := 0; i < 4; i++ {
		b.post("/login", url.Values{"password": {"wrong"}})
	}
	if rec := b.post("/login", url.Values{"password": {"correct horse"}}); rec.Code != 429 {
		t.Fatalf("sixth attempt in a minute must be limited even with the right password: %d", rec.Code)
	}
}

func TestSessionAndCSRF(t *testing.T) {
	_, api, b := setup(t)
	signIn(t, b)
	if rec := b.get("/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Matt&#39;s iPhone") {
		t.Fatalf("devices page: %d", rec.Code)
	}
	// A POST without the session's token is refused and does nothing.
	saved := b.csrf
	b.csrf = "forged"
	if rec := b.post("/devices/dev-a/revoke", url.Values{}); rec.Code != 403 {
		t.Fatalf("forged csrf: %d", rec.Code)
	}
	if len(api.revoked) != 0 {
		t.Fatal("a forged form reached the call server")
	}
	b.csrf = saved
	if rec := b.post("/logout", url.Values{}); rec.Code != 303 || b.cookie != nil {
		t.Fatalf("logout: %d", rec.Code)
	}
	if rec := b.get("/"); rec.Code != 303 {
		t.Fatal("still signed in after logout")
	}
}

func TestDevicesListAndDevicePage(t *testing.T) {
	_, _, b := setup(t)
	signIn(t, b)
	body := b.get("/").Body.String()
	for _, want := range []string{"dev-a", "Matt&#39;s iPhone", ">201<", "app · extension", "Enrolled", "Code issued", "dev-b", ">202<"} {
		if !strings.Contains(body, want) {
			t.Fatalf("devices page lacks %q", want)
		}
	}
	if !strings.Contains(b.get("/devices/new").Body.String(), "Add a device") {
		t.Fatal("add-device page")
	}
	page := b.get("/devices/dev-a")
	if page.Code != 200 {
		t.Fatalf("device page: %d", page.Code)
	}
	body = page.Body.String()
	for _, want := range []string{"sip:100@asterisk", "Matt</td>", "★", "Download CSV", "Danger zone", "Nothing set yet: the phone keeps", "/contacts/ct_1/edit", `data-confirm="Revoke`} {
		if !strings.Contains(body, want) {
			t.Fatalf("device page lacks %q", want)
		}
	}
	if copyPage := b.get("/devices/dev-a/directory/copy").Body.String(); !strings.Contains(copyPage, "ext 202") || !strings.Contains(copyPage, `value="dev-b"`) {
		t.Fatal("copy page must list the other devices")
	}
	if edit := b.get("/devices/dev-a/contacts/ct_2/edit").Body.String(); !strings.Contains(edit, `value="Matt"`) || !strings.Contains(edit, `name="favourite" checked`) || !strings.Contains(edit, "Delete this contact") {
		t.Fatal("edit page")
	}
	if add := b.get("/devices/dev-a/contacts/new").Body.String(); !strings.Contains(add, "Add a contact") || strings.Contains(add, "Delete this contact") {
		t.Fatal("add page")
	}
	if rec := b.get("/devices/dev-a/contacts/ct_9/edit"); rec.Code != 404 {
		t.Fatalf("unknown contact: %d", rec.Code)
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("no CSP")
	}
	if rec := b.get("/devices/nope"); rec.Code != 404 {
		t.Fatalf("unknown device: %d", rec.Code)
	}
}

func TestAddDeviceShowsTheCodeOnce(t *testing.T) {
	_, api, b := setup(t)
	signIn(t, b)
	rec := b.post("/devices", url.Values{"user": {"205"}, "label": {"Warehouse 3"}})
	if rec.Code != 303 || rec.Header().Get("Location") != "/devices/dev_NEW001/code" {
		t.Fatalf("create: %d → %s", rec.Code, rec.Header().Get("Location"))
	}
	page := b.get("/devices/dev_NEW001/code").Body.String()
	for _, want := range []string{"A7K2-M9PX", "<svg", "Warehouse 3", "dialler://enrol?c=A7K2M9PX"} {
		if !strings.Contains(page, want) {
			t.Fatalf("code page lacks %q", want)
		}
	}
	// Reloading does not show it again.
	if again := b.get("/devices/dev_NEW001/code").Body.String(); strings.Contains(again, "A7K2-M9PX") || !strings.Contains(again, "No code to show") {
		t.Fatal("the code must be shown once")
	}
	// A chosen id is used as given.
	if rec := b.post("/devices", url.Values{"device_id": {"warehouse-3"}, "user": {"206"}}); rec.Code != 303 || rec.Header().Get("Location") != "/devices/warehouse-3/code" {
		t.Fatalf("create with id: %d → %s", rec.Code, rec.Header().Get("Location"))
	}
	if api.devices[len(api.devices)-1].DeviceID != "warehouse-3" {
		t.Fatal("the chosen id did not reach the call server")
	}
	// A taken or malformed id comes back to the form with the reason and
	// what was typed, and nothing is created.
	n := len(api.devices)
	for _, bad := range []url.Values{
		{"device_id": {"dev-a"}, "user": {"207"}},
		{"device_id": {"../etc"}, "user": {"207"}},
		{"device_id": {""}, "user": {""}, "label": {"Kept"}},
	} {
		rec := b.post("/devices", bad)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `class="flash error"`) || !strings.Contains(rec.Body.String(), `value="`+bad.Get("user")+`"`) {
			t.Fatalf("bad create %v: %d", bad, rec.Code)
		}
	}
	if len(api.devices) != n {
		t.Fatal("a refused form reached the call server")
	}
	// A new code for an existing device.
	if rec := b.post("/devices/dev-a/code", url.Values{}); rec.Code != 303 {
		t.Fatalf("new code: %d", rec.Code)
	}
	if page := b.get("/devices/dev-a/code").Body.String(); !strings.Contains(page, "BFF6-GB4Z") {
		t.Fatal("new code not shown")
	}
	if api.minted != 3 {
		t.Fatalf("minted %d", api.minted)
	}
	// An empty extension never reaches the server.
	if rec := b.post("/devices", url.Values{"user": {" "}}); rec.Code != 422 || len(api.devices) != 4 {
		t.Fatalf("blank user: %d devices=%d", rec.Code, len(api.devices))
	}
}

// The device page is one form: only what changed reaches the server.
func TestDevicePageSavesInPlace(t *testing.T) {
	_, api, b := setup(t)
	signIn(t, b)
	page := b.get("/devices/dev-a").Body.String()
	for _, want := range []string{`value="Matt&#39;s iPhone"`, `value="201"`, `name="ssids"`, `name="pbx_user"`, "data-dirty-form", "data-directory", "<dialog"} {
		if !strings.Contains(page, want) {
			t.Fatalf("device page lacks %q", want)
		}
	}
	// Nothing changed: nothing called.
	rec := b.post("/devices/dev-a", url.Values{"label": {"Matt's iPhone"}, "user": {"201"}, "ssids": {""}, "pbx_user": {""}})
	if rec.Code != 303 || len(api.configs) != 0 || len(api.pbxCreds) != 0 || api.devices[0].User != "201" {
		t.Fatalf("no-op save touched something: %d %+v %+v", rec.Code, api.configs, api.pbxCreds)
	}
	if page := b.get("/devices/dev-a").Body.String(); !strings.Contains(page, "Nothing had changed.") {
		t.Fatal("no-op notice")
	}
	// Everything at once.
	rec = b.post("/devices/dev-a", url.Values{"label": {"Reception iPhone"}, "user": {"251"}, "ssids": {"Office\nOffice-5G"},
		"pbx_user": {"251"}, "pbx_password": {"s3cret"}, "pbx_device_name": {"SEP251"}})
	if rec.Code != 303 {
		t.Fatalf("save: %d", rec.Code)
	}
	if api.devices[0].Label != "Reception iPhone" || api.devices[0].User != "251" {
		t.Fatalf("device not updated: %+v", api.devices[0].Device)
	}
	if c := api.configs["dev-a"]; strings.Join(c.SSIDs, ",") != "Office,Office-5G" {
		t.Fatalf("networks: %+v", c)
	}
	if c := api.pbxCreds["dev-a"]; c.User != "251" || c.Password != "s3cret" || c.DeviceName != "SEP251" {
		t.Fatalf("pbx: %+v", c)
	}
	page = b.get("/devices/dev-a").Body.String()
	if !strings.Contains(page, "Saved: name and extension, networks, PBX registration.") || !strings.Contains(page, "registration pending") || strings.Contains(page, "s3cret") {
		t.Fatal("after save")
	}
	// A blank password keeps the stored one; a blanked username removes the registration.
	b.post("/devices/dev-a", url.Values{"label": {"Reception iPhone"}, "user": {"251"}, "ssids": {"Office\nOffice-5G"}, "pbx_user": {"cucm-251"}, "pbx_password": {""}, "pbx_device_name": {"SEP251"}})
	if c := api.pbxCreds["dev-a"]; c.User != "cucm-251" || c.Password != "s3cret" {
		t.Fatalf("username edit: %+v", c)
	}
	b.post("/devices/dev-a", url.Values{"label": {"Reception iPhone"}, "user": {"251"}, "ssids": {"Office\nOffice-5G"}, "pbx_user": {""}})
	if len(api.pbxCreds) != 0 {
		t.Fatal("blank username must remove the registration")
	}
	// First-time credentials need a password; an empty extension is refused.
	b.post("/devices/dev-a", url.Values{"label": {"Reception iPhone"}, "user": {"251"}, "ssids": {"Office\nOffice-5G"}, "pbx_user": {"x"}, "pbx_password": {""}})
	if len(api.pbxCreds) != 0 || !strings.Contains(b.get("/devices/dev-a").Body.String(), "password is needed") {
		t.Fatal("first-time credentials without a password")
	}
	if rec := b.post("/devices/dev-a", url.Values{"label": {"x"}, "user": {""}}); rec.Code != 303 || api.devices[0].User != "251" {
		t.Fatal("an empty extension must be refused before reaching the server")
	}
}

// The script saves contacts with fetch and expects JSON back.
func TestContactsAnswerJSONToFetch(t *testing.T) {
	_, api, b := setup(t)
	signIn(t, b)
	form := url.Values{"_csrf": {b.csrf}, "display_name": {"Echo"}, "uri": {"echo"}, "mode": {"local"}, "favourite": {"on"}}
	req := httptest.NewRequest("POST", "/devices/dev-a/contacts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	req.AddCookie(b.cookie)
	rec := httptest.NewRecorder()
	b.app.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"uri":"sip:echo@dialler"`) || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("json add: %d %s", rec.Code, rec.Body.String())
	}
	// Validation as JSON too.
	form.Set("display_name", "")
	req = httptest.NewRequest("POST", "/devices/dev-a/contacts", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	req.AddCookie(b.cookie)
	rec = httptest.NewRecorder()
	b.app.ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("json validation: %d %s", rec.Code, rec.Body.String())
	}
	// Delete with fetch.
	req = httptest.NewRequest("POST", "/devices/dev-a/contacts/ct_1/delete", strings.NewReader(url.Values{"_csrf": {b.csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "fetch")
	req.AddCookie(b.cookie)
	rec = httptest.NewRecorder()
	b.app.ServeHTTP(rec, req)
	if rec.Code != 200 || len(api.dirs["dev-a"]) != 2 {
		t.Fatalf("json delete: %d %s (%d left)", rec.Code, rec.Body.String(), len(api.dirs["dev-a"]))
	}
}

func TestRevokeNeedsConfirmation(t *testing.T) {
	_, api, b := setup(t)
	signIn(t, b)
	if page := b.get("/devices/dev-a/revoke").Body.String(); !strings.Contains(page, "Revoke dev-a?") {
		t.Fatal("confirm page")
	}
	if len(api.revoked) != 0 {
		t.Fatal("viewing the confirmation revoked")
	}
	if rec := b.post("/devices/dev-a/revoke", url.Values{}); rec.Code != 303 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if len(api.revoked) != 1 || api.revoked[0] != "dev-a" {
		t.Fatalf("revoked %v", api.revoked)
	}
	if page := b.get("/devices/dev-a").Body.String(); !strings.Contains(page, "Revoked. A new enrolment code") {
		t.Fatal("notice not shown after the redirect")
	}
	// Delete: its own confirmation, then the device is gone from the list.
	if page := b.get("/devices/dev-b/delete").Body.String(); !strings.Contains(page, "Delete dev-b?") || !strings.Contains(page, "no undo") {
		t.Fatal("delete confirm page")
	}
	if len(api.purged) != 0 {
		t.Fatal("viewing the confirmation purged")
	}
	if rec := b.post("/devices/dev-b/delete", url.Values{}); rec.Code != 303 || rec.Header().Get("Location") != "/" {
		t.Fatalf("delete: %d → %s", rec.Code, rec.Header().Get("Location"))
	}
	if len(api.purged) != 1 || api.purged[0] != "dev-b" {
		t.Fatalf("purged %v", api.purged)
	}
	if list := b.get("/").Body.String(); strings.Contains(list, ">dev-b<") || !strings.Contains(list, "Device deleted.") {
		t.Fatal("deleted device still listed, or no notice")
	}
}

func TestContactsSettingsAndCSV(t *testing.T) {
	_, api, b := setup(t)
	signIn(t, b)
	// Add with a bare number: completed with the server's domain.
	b.post("/devices/dev-a/contacts", url.Values{"display_name": {"Echo"}, "uri": {"echo"}, "mode": {"local"}, "favourite": {"on"}})
	added := api.dirs["dev-a"][2]
	if added.URI != "sip:echo@dialler" || !added.Favourite || added.Mode != directory.ModeLocal {
		t.Fatalf("added: %+v", added)
	}
	// Edit and delete.
	b.post("/devices/dev-a/contacts/ct_1", url.Values{"display_name": {"Front desk"}, "uri": {"sip:100@asterisk"}, "mode": {"trunk"}})
	if api.dirs["dev-a"][0].DisplayName != "Front desk" || api.dirs["dev-a"][0].Favourite {
		t.Fatalf("edited: %+v", api.dirs["dev-a"][0])
	}
	b.post("/devices/dev-a/contacts/ct_2/delete", url.Values{})
	if len(api.dirs["dev-a"]) != 2 {
		t.Fatalf("after delete: %d", len(api.dirs["dev-a"]))
	}
	// Settings: one per line or comma-separated, trimmed.
	b.post("/devices/dev-a/config", url.Values{"ssids": {"Office\n Office-5G , \n"}})
	if c := api.configs["dev-a"]; c.Version != 1 || strings.Join(c.SSIDs, ",") != "Office,Office-5G" {
		t.Fatalf("config: %+v", c)
	}
	if page := b.get("/devices/dev-a").Body.String(); !strings.Contains(page, "Office\nOffice-5G") || !strings.Contains(page, "Version 1") {
		t.Fatal("settings not shown")
	}
	// CSV download.
	rec := b.get("/devices/dev-a/directory.csv")
	if rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "display_name,uri,mode,favourite\n") || !strings.Contains(rec.Header().Get("Content-Disposition"), "dev-a-directory.csv") {
		t.Fatalf("csv: %d %q", rec.Code, rec.Body.String())
	}
	// CSV upload: preview first, nothing applied; then apply.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("_csrf", b.csrf)
	fw, _ := mw.CreateFormFile("file", "phones.csv")
	_, _ = io.WriteString(fw, "display_name,uri,mode,favourite\nFront desk,100@asterisk,trunk,false\nNew one,300,local,true\n")
	mw.Close()
	preview := b.do("POST", "/devices/dev-a/directory/upload", nil, &buf, mw.FormDataContentType())
	if preview.Code != 200 {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	body := preview.Body.String()
	for _, want := range []string{"phones.csv", "Added <span class=\"count\">1</span>", "New one", "Removed <span class=\"count\">1</span>", "sip:echo@dialler", "1 unchanged"} {
		if !strings.Contains(body, want) {
			t.Fatalf("preview lacks %q:\n%s", want, body)
		}
	}
	if api.replaced["dev-a"] != 0 {
		t.Fatal("preview must not apply")
	}
	if rec := b.post("/devices/dev-a/directory/apply", url.Values{}); rec.Code != 303 {
		t.Fatalf("apply: %d", rec.Code)
	}
	if api.replaced["dev-a"] != 1 || len(api.dirs["dev-a"]) != 2 || api.dirs["dev-a"][1].URI != "sip:300@dialler" {
		t.Fatalf("applied: %+v", api.dirs["dev-a"])
	}
	if rec := b.post("/devices/dev-a/directory/apply", url.Values{}); rec.Code != 303 || api.replaced["dev-a"] != 1 {
		t.Fatal("a second apply with nothing pending must do nothing")
	}
	// A bad file is refused whole, with the line.
	buf.Reset()
	mw = multipart.NewWriter(&buf)
	_ = mw.WriteField("_csrf", b.csrf)
	fw, _ = mw.CreateFormFile("file", "bad.csv")
	_, _ = io.WriteString(fw, "display_name,uri,mode\nA,1,local\nB,2,pigeon\n")
	mw.Close()
	if rec := b.do("POST", "/devices/dev-a/directory/upload", nil, &buf, mw.FormDataContentType()); rec.Code != 303 {
		t.Fatalf("bad csv: %d", rec.Code)
	}
	if page := b.get("/devices/dev-a").Body.String(); !strings.Contains(page, "line 3") {
		t.Fatal("the bad line is not reported")
	}
	// Copy to another device.
	b.post("/devices/dev-a/directory/copy", url.Values{"to": {"dev-b"}})
	if api.replaced["dev-b"] != 1 || len(api.dirs["dev-b"]) != 2 {
		t.Fatalf("copy: %v %d", api.replaced, len(api.dirs["dev-b"]))
	}
}

func TestServerPageAndPBXSettings(t *testing.T) {
	_, api, b := setup(t)
	signIn(t, b)
	body := b.get("/server").Body.String()
	for _, want := range []string{"10.0.0.1", "dialler", "signal 7443", "sip:asterisk:5060;transport=udp", "1 connected", `name="mode" value="register"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("server page lacks %q", want)
		}
	}
	// Invalid settings never reach the server.
	if rec := b.post("/server/pbx", url.Values{"mode": {"register"}, "host": {""}}); rec.Code != 303 || api.pbx != nil {
		t.Fatal("empty host must be refused")
	}
	if page := b.get("/server").Body.String(); !strings.Contains(page, "host is required") {
		t.Fatal("the validation error is not shown")
	}
	rec := b.post("/server/pbx", url.Values{"mode": {"register"}, "host": {"cucm.example"}, "port": {"5061"}, "transport": {"tls"},
		"expiry_seconds": {"600"}, "codecs": {"g722,pcmu"}, "srtp": {"sdes"}, "qualify_seconds": {"30"}, "tls_ca": {"/etc/dialler/cucm-ca.pem"}})
	if rec.Code != 303 || api.pbx == nil || api.pbx.Mode != pbxconfig.ModeRegister || api.pbx.Port != 5061 || api.pbx.TLSCA != "/etc/dialler/cucm-ca.pem" || api.pbx.ExpirySeconds != 600 {
		t.Fatalf("save: %d %+v", rec.Code, api.pbx)
	}
	page := b.get("/server").Body.String()
	if !strings.Contains(page, "next starts") || !strings.Contains(page, `value="cucm.example"`) || !strings.Contains(page, `value="register" checked`) {
		t.Fatal("saved settings not shown, or no restart notice")
	}
	if !strings.Contains(page, "differ") {
		t.Fatal("running trunk differs from the saved one: the page must say so")
	}
}
