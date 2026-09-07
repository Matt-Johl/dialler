package directory

import (
	"path/filepath"
	"testing"
)

// Re-seeding the directory (harness/provision.sh runs on every harness-up)
// must update the existing contact for a URI, not add another one.
func TestUpsertByURIDoesNotDuplicate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "directory.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Upsert(Contact{DisplayName: "Phone B", URI: "sip:202@dialler", Mode: ModeLocal})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Upsert(Contact{DisplayName: "Phone B (renamed)", URI: "SIP:202@dialler", Mode: ModeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("second upsert of the same URI created a new contact: %s vs %s", again.ID, first.ID)
	}
	contacts, _ := s.Changes(0)
	if len(contacts) != 1 {
		t.Fatalf("expected 1 live contact, got %d", len(contacts))
	}
	if contacts[0].DisplayName != "Phone B (renamed)" {
		t.Fatalf("upsert did not update the contact: %+v", contacts[0])
	}

	// A deleted contact does not capture the URI: a new one is created.
	if _, err := s.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	third, err := s.Upsert(Contact{DisplayName: "Phone B", URI: "sip:202@dialler", Mode: ModeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if third.ID == first.ID {
		t.Fatal("upsert revived a deleted contact's id")
	}
}
