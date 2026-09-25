package directory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A device's file carries the schema; a pre-9b file without one reads as
// schema 1 and is rewritten with it; a newer one is refused.
func TestFileSchema(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"version":3,"contacts":{"ct_01":{"id":"ct_01","display_name":"A","uri":"sip:1@x","mode":"local","version":3,"updated_at":"2026-09-01T00:00:00Z"}}}`
	_ = os.WriteFile(filepath.Join(dir, "dev-a.json"), []byte(legacy), 0o600)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if contacts, v := s.Contacts("dev-a"); v != 3 || len(contacts) != 1 {
		t.Fatalf("legacy file: v%d %+v", v, contacts)
	}
	if _, err := s.Upsert("dev-a", Contact{DisplayName: "B", URI: "sip:2@x", Mode: ModeLocal}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "dev-a.json"))
	var st struct {
		Schema  int `json:"schema"`
		Version int64
	}
	if err := json.Unmarshal(raw, &st); err != nil || st.Schema != 1 || st.Version != 4 {
		t.Fatalf("rewritten: %s", raw)
	}
	_ = os.WriteFile(filepath.Join(dir, "dev-b.json"), []byte(`{"schema":2,"version":1,"contacts":{}}`), 0o600)
	if _, err := Open(dir); err == nil {
		t.Fatal("a newer schema must be refused")
	}
}

// A failed write restores memory (ADMIN-API.md §4.4): the entry is
// unchanged and nobody is notified.
func TestFailedWriteRestoresMemory(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := t.TempDir()
	s, _ := Open(dir)
	var notified int
	s.OnChange(func(string, int64) { notified++ })
	c, err := s.Upsert("dev-a", Contact{DisplayName: "A", URI: "sip:1@x", Mode: ModeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o700)

	if _, err := s.Upsert("dev-a", Contact{DisplayName: "B", URI: "sip:2@x", Mode: ModeLocal}); err == nil {
		t.Fatal("the write should have failed")
	}
	if _, err := s.Replace("dev-a", []Contact{{DisplayName: "C", URI: "sip:3@x", Mode: ModeLocal}}); err == nil {
		t.Fatal("the replace should have failed")
	}
	if _, err := s.Delete("dev-a", c.ID); err == nil {
		t.Fatal("the delete should have failed")
	}
	if _, err := s.Upsert("dev-new", Contact{DisplayName: "N", URI: "sip:9@x", Mode: ModeLocal}); err == nil {
		t.Fatal("the write should have failed")
	}
	contacts, v := s.Contacts("dev-a")
	if v != 1 || len(contacts) != 1 || contacts[0].ID != c.ID || contacts[0].DisplayName != "A" || contacts[0].Deleted {
		t.Fatalf("memory changed after failed writes: v%d %+v", v, contacts)
	}
	if s.Version("dev-new") != 0 {
		t.Fatal("a directory that could not be written exists in memory")
	}
	if notified != 1 {
		t.Fatalf("notified %d times; only the successful write may notify", notified)
	}
}

// Every write names its writer for the directory_changed event: the admin,
// the device, or the server's own migration (ADMIN-API.md §5.9).
func TestOnWriteNamesTheWriter(t *testing.T) {
	s, _ := Open("")
	var got []string
	s.OnWrite(func(_ string, _ int64, by string) { got = append(got, by) })
	var changes int
	s.OnChange(func(string, int64) { changes++ })
	c, _ := s.Upsert("dev-a", Contact{URI: "sip:1@x", Mode: ModeLocal}, By("device"))
	s.Replace("dev-a", []Contact{{URI: "sip:2@x", Mode: ModeLocal}}, By("admin"))
	s.Delete("dev-a", c.ID, By("admin")) // already tombstoned by the replace: no write, no event
	s.Upsert("dev-a", Contact{URI: "sip:3@x", Mode: ModeLocal}, By("server"), Match{Version: 2})
	s.Upsert("dev-a", Contact{URI: "sip:4@x", Mode: ModeLocal}) // no By: ""
	want := []string{"device", "admin", "server", ""}
	if len(got) != len(want) {
		t.Fatalf("writers %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("writers %v, want %v", got, want)
		}
	}
	if changes != len(got) {
		t.Fatalf("OnChange fired %d times, OnWrite %d", changes, len(got))
	}
}
