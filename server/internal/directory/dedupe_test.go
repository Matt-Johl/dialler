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

// A directory that already accumulated duplicates for a URI (created before
// the dedupe logic, each with its own ID) must collapse to a single live
// contact on the next re-seed, with the extras tombstoned so clients delete
// them on their next sync.
func TestUpsertCollapsesExistingDuplicates(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "directory.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Five live contacts for one URI, each with a distinct explicit ID (the
	// pre-dedupe accumulation this heals).
	for i, id := range []string{"ct_a", "ct_b", "ct_c", "ct_d", "ct_e"} {
		if _, err := s.Upsert(Contact{ID: id, DisplayName: "Matt", URI: "sip:201@dialler", Mode: ModeLocal}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	before, _ := s.Changes(0)
	if len(before) != 5 {
		t.Fatalf("setup expected 5 live duplicates, got %d", len(before))
	}
	sinceCollapse := s.Version()

	// A re-seed (empty ID, provision.sh style) collapses them to one.
	if _, err := s.Upsert(Contact{DisplayName: "Matt (201)", URI: "sip:201@dialler", Mode: ModeLocal}); err != nil {
		t.Fatal(err)
	}
	live, _ := s.Changes(0)
	if len(live) != 1 {
		t.Fatalf("expected 1 live contact after collapse, got %d", len(live))
	}
	if live[0].DisplayName != "Matt (201)" {
		t.Fatalf("collapse kept the wrong contact: %+v", live[0])
	}
	// The four extras must reach a client that synced before the collapse as
	// tombstones so it removes its duplicates.
	delta, _ := s.Changes(sinceCollapse)
	tombstones := 0
	for _, c := range delta {
		if c.Deleted {
			tombstones++
		}
	}
	if tombstones != 4 {
		t.Fatalf("expected 4 tombstones in the delta, got %d (delta %d)", tombstones, len(delta))
	}
}
