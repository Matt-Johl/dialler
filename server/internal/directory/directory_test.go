package directory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type change struct {
	device  string
	version int64
}

func TestDeltaSyncAndPersistencePerDevice(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "directories")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	var notified []change
	s.OnChange(func(d string, v int64) { notified = append(notified, change{d, v}) })

	a, _ := s.Upsert("dev-a", Contact{DisplayName: "Reception", URI: "sip:100@pbx", Mode: ModeTrunk})
	b, _ := s.Upsert("dev-a", Contact{DisplayName: "Matt", URI: "sip:201@dialler", Mode: ModeLocal, Favourite: true})
	if a.Version != 1 || b.Version != 2 || !strings.HasPrefix(a.ID, "ct_") || !b.Favourite {
		t.Fatalf("versions/ids: %+v %+v", a, b)
	}
	if _, err := s.Upsert("dev-a", Contact{URI: "x", Mode: "carrier-pigeon"}); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
	if _, err := s.Upsert("", Contact{URI: "sip:1@x", Mode: ModeLocal}); err != ErrNoDevice {
		t.Fatalf("want ErrNoDevice, got %v", err)
	}

	full, v := s.Changes("dev-a", 0)
	if v != 2 || len(full) != 2 || full[0].ID != a.ID {
		t.Fatalf("full sync: v=%d %+v", v, full)
	}
	// Another device has nothing: its own directory, its own version.
	if other, v := s.Changes("dev-b", 0); v != 0 || len(other) != 0 {
		t.Fatalf("dev-b should be empty: v=%d %+v", v, other)
	}
	if s.Version("dev-b") != 0 || s.Version("dev-a") != 2 {
		t.Fatalf("versions: a=%d b=%d", s.Version("dev-a"), s.Version("dev-b"))
	}

	// Client holding v2 sees nothing; then a delete arrives as a tombstone.
	if delta, _ := s.Changes("dev-a", 2); len(delta) != 0 {
		t.Fatalf("no-op delta returned %+v", delta)
	}
	if ok, _ := s.Delete("dev-a", a.ID); !ok {
		t.Fatal("delete")
	}
	if ok, _ := s.Delete("dev-a", a.ID); ok {
		t.Fatal("double delete reported success")
	}
	if ok, _ := s.Delete("dev-b", a.ID); ok {
		t.Fatal("a delete on another device's directory found the contact")
	}
	delta, v := s.Changes("dev-a", 2)
	if v != 3 || len(delta) != 1 || !delta[0].Deleted || delta[0].ID != a.ID {
		t.Fatalf("tombstone delta: v=%d %+v", v, delta)
	}
	if full, _ := s.Changes("dev-a", 0); len(full) != 1 || full[0].ID != b.ID {
		t.Fatalf("full sync after delete: %+v", full)
	}
	want := []change{{"dev-a", 1}, {"dev-a", 2}, {"dev-a", 3}}
	if len(notified) != 3 || notified[0] != want[0] || notified[2] != want[2] {
		t.Fatalf("onChange: %v", notified)
	}

	// One file per device; a reload keeps version, favourite and tombstone.
	if _, err := os.Stat(filepath.Join(dir, "dev-a.json")); err != nil {
		t.Fatalf("dev-a's file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dev-b.json")); !os.IsNotExist(err) {
		t.Fatalf("dev-b never changed, so it must have no file: %v", err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Version("dev-a") != 3 || len(s2.Devices()) != 1 {
		t.Fatalf("persisted: v=%d devices=%v", s2.Version("dev-a"), s2.Devices())
	}
	if delta, _ := s2.Changes("dev-a", 2); len(delta) != 1 || !delta[0].Deleted {
		t.Fatalf("persisted tombstone missing: %+v", delta)
	}
	if live, _ := s2.Contacts("dev-a"); len(live) != 1 || !live[0].Favourite {
		t.Fatalf("persisted favourite missing: %+v", live)
	}
}

// A write to one device's directory changes that device's file and
// version and notifies that device — and nothing of any other device's
// (SPEC §4.8 isolation).
func TestWritesAreIsolatedPerDevice(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "directories")
	s, _ := Open(dir)
	var notified []change
	s.OnChange(func(d string, v int64) { notified = append(notified, change{d, v}) })
	s.Upsert("dev-a", Contact{DisplayName: "A's", URI: "sip:100@pbx", Mode: ModeTrunk})
	s.Upsert("dev-b", Contact{DisplayName: "B's", URI: "sip:100@pbx", Mode: ModeTrunk})
	before, _ := os.ReadFile(filepath.Join(dir, "dev-b.json"))

	s.Upsert("dev-a", Contact{DisplayName: "A's, renamed", URI: "sip:100@pbx", Mode: ModeTrunk})
	s.Replace("dev-a", nil) // A's whole directory removed

	after, _ := os.ReadFile(filepath.Join(dir, "dev-b.json"))
	if string(before) != string(after) {
		t.Fatal("dev-b's file changed when only dev-a was written")
	}
	if s.Version("dev-b") != 1 || s.Version("dev-a") != 3 {
		t.Fatalf("versions: a=%d b=%d", s.Version("dev-a"), s.Version("dev-b"))
	}
	if live, _ := s.Contacts("dev-b"); len(live) != 1 || live[0].DisplayName != "B's" {
		t.Fatalf("dev-b's contact touched: %+v", live)
	}
	for _, n := range notified[2:] {
		if n.device != "dev-a" {
			t.Fatalf("dev-b was notified of dev-a's change: %v", notified)
		}
	}
	if err := s.Purge("dev-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dev-a.json")); !os.IsNotExist(err) {
		t.Fatal("purge left dev-a's file")
	}
	if s.Version("dev-b") != 1 {
		t.Fatal("purge of dev-a touched dev-b")
	}
}

// Replace is the CSV upload: the list given becomes the directory, in one
// version bump, and a client that held the old version syncs the whole
// change as one delta.
func TestReplaceReconcilesByURIInOneVersion(t *testing.T) {
	s, _ := Open("")
	var notified []change
	s.OnChange(func(d string, v int64) { notified = append(notified, change{d, v}) })
	s.Upsert("dev-a", Contact{DisplayName: "Keep", URI: "sip:1@x", Mode: ModeLocal})
	s.Upsert("dev-a", Contact{DisplayName: "Rename me", URI: "sip:2@x", Mode: ModeLocal})
	s.Upsert("dev-a", Contact{DisplayName: "Drop me", URI: "sip:3@x", Mode: ModeLocal})
	synced := s.Version("dev-a") // a client holding v3

	res, err := s.Replace("dev-a", []Contact{
		{DisplayName: "Keep", URI: "sip:1@x", Mode: ModeLocal},
		{DisplayName: "Renamed", URI: "SIP:2@x", Mode: ModeLocal, Favourite: true},
		{DisplayName: "New", URI: "sip:4@x", Mode: ModeTrunk},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 4 || res.Added != 1 || res.Changed != 1 || res.Removed != 1 {
		t.Fatalf("replace result: %+v", res)
	}
	live, v := s.Contacts("dev-a")
	names := []string{}
	for _, c := range live {
		names = append(names, c.DisplayName)
	}
	sort.Strings(names)
	if v != 4 || strings.Join(names, ",") != "Keep,New,Renamed" {
		t.Fatalf("after replace: v=%d %v", v, names)
	}
	delta, _ := s.Changes("dev-a", synced)
	if len(delta) != 3 {
		t.Fatalf("the client should see exactly the rename, the tombstone and the new one: %+v", delta)
	}
	for _, c := range delta {
		if c.Version != 4 {
			t.Fatalf("every change carries the one new version: %+v", c)
		}
	}
	if len(notified) != 4 || notified[3] != (change{"dev-a", 4}) {
		t.Fatalf("one notification for the lot: %v", notified)
	}
	// The same list again is a no-op: no version bump, no notification.
	res, _ = s.Replace("dev-a", []Contact{
		{DisplayName: "Keep", URI: "sip:1@x", Mode: ModeLocal},
		{DisplayName: "Renamed", URI: "sip:2@x", Mode: ModeLocal, Favourite: true},
		{DisplayName: "New", URI: "sip:4@x", Mode: ModeTrunk},
	})
	if res.Version != 4 || res.Added+res.Changed+res.Removed != 0 || len(notified) != 4 {
		t.Fatalf("identical replace was not a no-op: %+v notified=%v", res, notified)
	}
	if _, err := s.Replace("dev-a", []Contact{{URI: "", Mode: ModeLocal}}); err != ErrInvalid {
		t.Fatalf("invalid row must reject the whole upload: %v", err)
	}
}

// The pre-item-7 global directory.json (this is its exact shape, with a
// tombstone and the version counter) is folded into every enrolled
// device's directory once, then renamed so it never runs again.
func TestMigrateLegacyGlobalDirectory(t *testing.T) {
	data := t.TempDir()
	legacy := filepath.Join(data, "directory.json")
	const fixture = `{
  "version": 702,
  "contacts": {
    "ct_0a1b": {"id": "ct_0a1b", "display_name": "Matt (201)", "uri": "sip:201@dialler", "mode": "local", "version": 700, "updated_at": "2026-09-05T10:00:00Z"},
    "ct_2c3d": {"id": "ct_2c3d", "display_name": "Desk phone (100)", "uri": "sip:100@asterisk", "mode": "trunk", "version": 701, "updated_at": "2026-09-05T10:00:01Z"},
    "ct_dead": {"id": "ct_dead", "display_name": "Gone", "uri": "sip:999@asterisk", "mode": "trunk", "version": 702, "updated_at": "2026-09-05T10:00:02Z", "deleted": true}
  }
}`
	if err := os.WriteFile(legacy, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(data, "directories"))
	if err != nil {
		t.Fatal(err)
	}
	// dev-b already has one of the contacts (under another name): the
	// migration updates it by URI rather than duplicating it.
	s.Upsert("dev-b", Contact{DisplayName: "My Matt", URI: "sip:201@dialler", Mode: ModeLocal, Favourite: true})

	n, m, err := s.Migrate(legacy, []string{"dev-a", "dev-b"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || m != 2 {
		t.Fatalf("migrated %d contacts into %d devices", n, m)
	}
	for _, dev := range []string{"dev-a", "dev-b"} {
		live, _ := s.Contacts(dev)
		if len(live) != 2 {
			t.Fatalf("%s: %d live contacts, want 2 (the tombstone stays dead): %+v", dev, len(live), live)
		}
	}
	if live, _ := s.Contacts("dev-b"); live[0].URI != "sip:201@dialler" || live[0].DisplayName != "Matt (201)" || !live[0].Favourite {
		t.Fatalf("dev-b's existing contact should be updated in place, keeping its favourite: %+v", live[0])
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy file still in place after migration")
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("legacy file not kept as .migrated: %v", err)
	}
	// Running again finds nothing to do.
	if n, m, err := s.Migrate(legacy, []string{"dev-a"}); err != nil || n != 0 || m != 0 {
		t.Fatalf("second migration: %d %d %v", n, m, err)
	}
	// With nobody to receive it the file is left alone.
	if err := os.Rename(legacy+".migrated", legacy); err != nil {
		t.Fatal(err)
	}
	if n, m, err := s.Migrate(legacy, nil); err != nil || n != 0 || m != 0 {
		t.Fatalf("migration with no devices: %d %d %v", n, m, err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatal("legacy file must stay when no device can receive it")
	}
}

func TestDeviceHandler(t *testing.T) {
	s, _ := Open("")
	passAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Device-ID")
			if id != "dev1" && id != "dev2" {
				http.Error(w, "unauthorized", 401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := NewHandler(s, passAuth)

	// Handlers are exercised via ServeHTTP + a recorder: no sockets needed.
	do := func(method, path, body string, hdr map[string]string) (*http.Response, []byte) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}
	dev1 := map[string]string{"X-Device-ID": "dev1"}
	dev2 := map[string]string{"X-Device-ID": "dev2"}

	if resp, _ := do("POST", "/v1/directory", `{"uri":"sip:100@pbx","mode":"trunk"}`, nil); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated write: %d", resp.StatusCode)
	}
	if resp, _ := do("GET", "/v1/directory", "", nil); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated read: %d", resp.StatusCode)
	}
	// The device writes its own directory.
	resp, body := do("POST", "/v1/directory", `{"display_name":"Reception","uri":"sip:100@pbx","mode":"trunk"}`, dev1)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var c Contact
	_ = json.Unmarshal(body, &c)
	if resp, _ := do("PUT", "/v1/directory/"+c.ID, `{"display_name":"Front Desk","uri":"sip:100@pbx","mode":"trunk","favourite":true}`, dev1); resp.StatusCode != 200 {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	if resp, _ := do("PUT", "/v1/directory/x", `{"uri":"","mode":"trunk"}`, dev1); resp.StatusCode != 400 {
		t.Fatalf("invalid contact: %d", resp.StatusCode)
	}
	resp, body = do("GET", "/v1/directory?since=0", "", dev1)
	var sync SyncResponse
	_ = json.Unmarshal(body, &sync)
	if resp.StatusCode != 200 || sync.Version != 2 || len(sync.Contacts) != 1 || sync.Contacts[0].DisplayName != "Front Desk" || !sync.Contacts[0].Favourite {
		t.Fatalf("sync: %d %s", resp.StatusCode, body)
	}
	// Another device sees none of it, and cannot touch it by id.
	_, body = do("GET", "/v1/directory?since=0", "", dev2)
	_ = json.Unmarshal(body, &sync)
	if sync.Version != 0 || len(sync.Contacts) != 0 {
		t.Fatalf("dev2 sees dev1's directory: %s", body)
	}
	if resp, _ := do("DELETE", "/v1/directory/"+c.ID, "", dev2); resp.StatusCode != 404 {
		t.Fatalf("dev2 deleting dev1's contact: %d", resp.StatusCode)
	}
	if resp, _ := do("DELETE", "/v1/directory/"+c.ID, "", dev1); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := do("DELETE", "/v1/directory/"+c.ID, "", dev1); resp.StatusCode != 404 {
		t.Fatalf("re-delete: %d", resp.StatusCode)
	}
	_, body = do("GET", "/v1/directory?since=2", "", dev1)
	_ = json.Unmarshal(body, &sync)
	if sync.Version != 3 || len(sync.Contacts) != 1 || !sync.Contacts[0].Deleted {
		t.Fatalf("tombstone via API: %s", body)
	}
}

func TestAdminHandler(t *testing.T) {
	s, _ := Open("")
	known := func(id string) bool { return id == "dev-a" || id == "dev-b" }
	h := NewAdminHandler(s, "admin", known)
	do := func(method, path, body string, auth bool) (*http.Response, []byte) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer admin")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}

	if resp, _ := do("GET", "/v1/admin/devices/dev-a/directory", "", false); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := do("POST", "/v1/admin/devices/dev-zz/directory", `{"uri":"sip:1@x","mode":"local"}`, true); resp.StatusCode != 404 {
		t.Fatalf("unknown device must not get a directory: %d", resp.StatusCode)
	}
	resp, body := do("POST", "/v1/admin/devices/dev-a/directory", `{"display_name":"One","uri":"sip:1@x","mode":"local"}`, true)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var c Contact
	_ = json.Unmarshal(body, &c)
	if resp, _ := do("PUT", "/v1/admin/devices/dev-a/directory/"+c.ID, `{"display_name":"One!","uri":"sip:1@x","mode":"local"}`, true); resp.StatusCode != 200 {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	// Replace-all (the CSV upload).
	resp, body = do("PUT", "/v1/admin/devices/dev-a/directory",
		`{"contacts":[{"display_name":"One!","uri":"sip:1@x","mode":"local"},{"display_name":"Two","uri":"sip:2@x","mode":"trunk","favourite":true}]}`, true)
	var res ReplaceResult
	_ = json.Unmarshal(body, &res)
	if resp.StatusCode != 200 || res.Added != 1 || res.Changed != 0 || res.Removed != 0 || res.Version != 3 {
		t.Fatalf("replace: %d %s", resp.StatusCode, body)
	}
	if resp, body := do("PUT", "/v1/admin/devices/dev-a/directory", `{"contacts":[{"uri":"","mode":"local"}]}`, true); resp.StatusCode != 400 {
		t.Fatalf("bad row: %d %s", resp.StatusCode, body)
	}
	resp, body = do("GET", "/v1/admin/devices/dev-a/directory", "", true)
	var list ListResponse
	_ = json.Unmarshal(body, &list)
	if resp.StatusCode != 200 || list.Version != 3 || len(list.Contacts) != 2 || !list.Contacts[1].Favourite {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do("DELETE", "/v1/admin/devices/dev-a/directory/"+c.ID, "", true); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, body := do("GET", "/v1/admin/devices/dev-b/directory", "", true); resp.StatusCode != 200 || !strings.Contains(string(body), `"contacts":[]`) {
		t.Fatalf("dev-b untouched: %d %s", resp.StatusCode, body)
	}
}
