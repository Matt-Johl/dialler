package licence

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestInstallIDIsMintedOnceAndKept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "install.id")
	id, err := LoadOrCreateInstallID(path)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("minted id %q is not 32 hex characters", id)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("install.id mode %o, want 0600", st.Mode().Perm())
	}
	again, err := LoadOrCreateInstallID(path)
	if err != nil {
		t.Fatal(err)
	}
	if again != id {
		t.Fatalf("second load %q != first %q; the id must never change", again, id)
	}
	// Whitespace around a hand-copied id is tolerated; the value is not.
	if err := os.WriteFile(path, []byte("  "+id+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadOrCreateInstallID(path); got != id {
		t.Fatalf("trimmed load %q != %q", got, id)
	}
}

func TestAMalformedInstallIDIsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.id")
	for _, bad := range []string{"", "short", "has space in it 0123456789abcdef", "ünïcödé-0123456789abcdef0123456789"} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateInstallID(path); err == nil {
			t.Errorf("%q: accepted; a malformed id must stop the server, not mint a new one", bad)
		}
	}
	// And it must not have been overwritten by a fresh one.
	b, _ := os.ReadFile(path)
	if string(b) != "ünïcödé-0123456789abcdef0123456789" {
		t.Error("a malformed install.id was rewritten")
	}
}
