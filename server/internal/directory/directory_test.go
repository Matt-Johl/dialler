package directory

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeltaSyncAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "directory.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var notified []int64
	s.OnChange(func(v int64) { notified = append(notified, v) })

	a, _ := s.Upsert(Contact{DisplayName: "Reception", URI: "sip:100@pbx", Mode: ModeTrunk})
	b, _ := s.Upsert(Contact{DisplayName: "Matt", URI: "sip:201@dialler", Mode: ModeLocal})
	if a.Version != 1 || b.Version != 2 || !strings.HasPrefix(a.ID, "ct_") {
		t.Fatalf("versions/ids: %+v %+v", a, b)
	}
	if _, err := s.Upsert(Contact{URI: "x", Mode: "carrier-pigeon"}); err != ErrInvalid {
		t.Fatalf("want ErrInvalid, got %v", err)
	}

	full, v := s.Changes(0)
	if v != 2 || len(full) != 2 || full[0].ID != a.ID {
		t.Fatalf("full sync: v=%d %+v", v, full)
	}

	// Client holding v2 sees nothing; then a delete arrives as a tombstone.
	if delta, _ := s.Changes(2); len(delta) != 0 {
		t.Fatalf("no-op delta returned %+v", delta)
	}
	if ok, _ := s.Delete(a.ID); !ok {
		t.Fatal("delete")
	}
	if ok, _ := s.Delete(a.ID); ok {
		t.Fatal("double delete reported success")
	}
	delta, v := s.Changes(2)
	if v != 3 || len(delta) != 1 || !delta[0].Deleted || delta[0].ID != a.ID {
		t.Fatalf("tombstone delta: v=%d %+v", v, delta)
	}
	// Full sync hides tombstones.
	if full, _ := s.Changes(0); len(full) != 1 || full[0].ID != b.ID {
		t.Fatalf("full sync after delete: %+v", full)
	}
	if len(notified) != 3 || notified[2] != 3 {
		t.Fatalf("onChange: %v", notified)
	}

	// Reload from disk keeps version and tombstone.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Version() != 3 {
		t.Fatalf("persisted version %d", s2.Version())
	}
	if delta, _ := s2.Changes(2); len(delta) != 1 || !delta[0].Deleted {
		t.Fatalf("persisted tombstone missing: %+v", delta)
	}
}

func TestHandler(t *testing.T) {
	s, _ := Open("")
	passAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Device-ID") != "dev1" {
				http.Error(w, "unauthorized", 401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := NewHandler(s, passAuth, "admin")

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
	adminH := map[string]string{"Authorization": "Bearer admin"}
	devH := map[string]string{"X-Device-ID": "dev1"}

	if resp, _ := do("POST", "/v1/directory", `{"uri":"sip:100@pbx","mode":"trunk"}`, nil); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated write: %d", resp.StatusCode)
	}
	resp, body := do("POST", "/v1/directory", `{"display_name":"Reception","uri":"sip:100@pbx","mode":"trunk"}`, adminH)
	if resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var c Contact
	_ = json.Unmarshal(body, &c)
	if resp, _ := do("PUT", "/v1/directory/"+c.ID, `{"display_name":"Front Desk","uri":"sip:100@pbx","mode":"trunk"}`, adminH); resp.StatusCode != 200 {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	if resp, _ := do("PUT", "/v1/directory/x", `{"uri":"","mode":"trunk"}`, adminH); resp.StatusCode != 400 {
		t.Fatalf("invalid contact: %d", resp.StatusCode)
	}

	if resp, _ := do("GET", "/v1/directory", "", nil); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated read: %d", resp.StatusCode)
	}
	resp, body = do("GET", "/v1/directory?since=0", "", devH)
	var sync SyncResponse
	_ = json.Unmarshal(body, &sync)
	if resp.StatusCode != 200 || sync.Version != 2 || len(sync.Contacts) != 1 || sync.Contacts[0].DisplayName != "Front Desk" {
		t.Fatalf("sync: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do("DELETE", "/v1/directory/"+c.ID, "", adminH); resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := do("DELETE", "/v1/directory/"+c.ID, "", adminH); resp.StatusCode != 404 {
		t.Fatalf("re-delete: %d", resp.StatusCode)
	}
	_, body = do("GET", "/v1/directory?since=2", "", devH)
	_ = json.Unmarshal(body, &sync)
	if sync.Version != 3 || len(sync.Contacts) != 1 || !sync.Contacts[0].Deleted {
		t.Fatalf("tombstone via API: %s", body)
	}
}
