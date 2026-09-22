package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealAndOpenRoundTrip(t *testing.T) {
	b, err := OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := b.Seal("dev-a", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "hunter2") {
		t.Fatalf("the plaintext survived into %q", sealed)
	}
	got, err := b.Open("dev-a", sealed)
	if err != nil || got != "hunter2" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestASealedValueCannotBeMovedBetweenDevices(t *testing.T) {
	b, err := OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := b.Seal("dev-a", "hunter2")
	// Editing devices.json to give one device another's credential must not
	// work: the device id is bound into the ciphertext.
	if _, err := b.Open("dev-b", sealed); err == nil {
		t.Fatal("a sealed secret opened under another device's label")
	}
}

func TestTheKeyIsKeptAndReusedAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "pbx.key")
	b1, err := OpenKey(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := b1.Seal("dev-a", "hunter2")

	// A restart must be able to read what the last run wrote — otherwise
	// every line on the fleet would need re-provisioning after a bounce.
	b2, err := OpenKey(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b2.Open("dev-a", sealed)
	if err != nil || got != "hunter2" {
		t.Fatalf("after reopening the key, Open = %q, %v", got, err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
}

func TestAnotherKeyCannotOpenIt(t *testing.T) {
	a, _ := OpenKey(filepath.Join(t.TempDir(), "a.key"))
	b, _ := OpenKey(filepath.Join(t.TempDir(), "b.key"))
	sealed, _ := a.Seal("dev-a", "hunter2")
	if _, err := b.Open("dev-a", sealed); err == nil {
		t.Fatal("a different key opened the value")
	}
}

func TestRejectsRubbish(t *testing.T) {
	b, _ := OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	for _, in := range []string{"", "hunter2", "v1:not-base64!!", "v1:AAAA"} {
		if _, err := b.Open("dev-a", in); err == nil {
			t.Errorf("Open(%q) should have failed", in)
		}
	}
	var nilBox *Box
	if _, err := nilBox.Seal("dev-a", "x"); err == nil {
		t.Error("sealing without a key should fail rather than store plaintext")
	}
}

func TestABadKeyFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pbx.key")
	if err := os.WriteFile(path, []byte("not hex"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenKey(path); err == nil {
		t.Fatal("expected an error for a key file that is not a hex key")
	}
	if err := os.WriteFile(path, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenKey(path); err == nil {
		t.Fatal("expected an error for a key of the wrong length")
	}
}
