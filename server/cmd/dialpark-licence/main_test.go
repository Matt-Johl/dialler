package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dialpark/server/internal/licence"
)

func TestKeygenIssueInspect(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := run([]string{"keygen", "-out", dir}, &out, &errb); code != 0 {
		t.Fatalf("keygen exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "vendorPublicKeyBase64") || !strings.Contains(out.String(), "vendorPublicKey = Data") {
		t.Fatalf("keygen must print both literals to paste:\n%s", out.String())
	}
	st, err := os.Stat(filepath.Join(dir, "vendor.key"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("vendor.key: %v mode %v", err, st.Mode())
	}
	// A second keygen into the same directory is refused: it would strand
	// every licence issued under the first.
	if code := run([]string{"keygen", "-out", dir}, &out, &errb); code == 0 {
		t.Fatal("keygen overwrote an existing key pair")
	}

	out.Reset()
	errb.Reset()
	code := run([]string{"issue", "-key", filepath.Join(dir, "vendor.key"), "-customer", "Example Ltd",
		"-install-id", "0123456789abcdef0123456789abcdef", "-seats", "25", "-valid-until", "2036-12-31"}, &out, &errb)
	if code != 0 {
		t.Fatalf("issue exit %d: %s", code, errb.String())
	}
	tok := strings.TrimSpace(out.String())
	if !strings.HasPrefix(tok, "DP1.") {
		t.Fatalf("issue printed %q", tok)
	}

	// The compiled-in key is empty in a test build, so inspect must be told
	// which public key to use; with it the token verifies.
	out.Reset()
	errb.Reset()
	if code := run([]string{"inspect", "-pub", filepath.Join(dir, "vendor.pub"), tok}, &out, &errb); code != 0 {
		t.Fatalf("inspect exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"Example Ltd", "0123456789abcdef0123456789abcdef", "seats       25", "2037-01-01T00:00:00Z", "verified"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("inspect output lacks %q:\n%s", want, out.String())
		}
	}
	// And the same token parses in the package with that key, which is
	// exactly what the server does.
	pub, err := loadPub(filepath.Join(dir, "vendor.pub"))
	if err != nil {
		t.Fatal(err)
	}
	licence.PublicKey = pub
	l, err := licence.Parse(tok)
	if err != nil {
		t.Fatal(err)
	}
	if l.Seats != 25 || l.Customer != "Example Ltd" || !strings.HasPrefix(l.ID, "lic_") || len(l.ID) != 12 {
		t.Fatalf("parsed %+v", l)
	}

	// A token edited after issue does not inspect.
	bad := tok[:len(tok)-2] + "AA"
	if code := run([]string{"inspect", "-pub", filepath.Join(dir, "vendor.pub"), bad}, &out, &errb); code == 0 {
		t.Fatal("inspect verified an edited token")
	}
}

func TestIssueRefusesIncompleteArguments(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	run([]string{"keygen", "-out", dir}, &out, &errb)
	key := filepath.Join(dir, "vendor.key")
	for name, args := range map[string][]string{
		"no customer":  {"issue", "-key", key, "-install-id", "x", "-seats", "1", "-valid-until", "2030-01-01"},
		"no install":   {"issue", "-key", key, "-customer", "C", "-seats", "1", "-valid-until", "2030-01-01"},
		"zero seats":   {"issue", "-key", key, "-customer", "C", "-install-id", "x", "-seats", "0", "-valid-until", "2030-01-01"},
		"bad date":     {"issue", "-key", key, "-customer", "C", "-install-id", "x", "-seats", "1", "-valid-until", "soon"},
		"missing key":  {"issue", "-key", filepath.Join(dir, "nope"), "-customer", "C", "-install-id", "x", "-seats", "1", "-valid-until", "2030-01-01"},
		"unknown cmd":  {"frobnicate"},
		"no arguments": {},
		"inspect none": {"inspect"},
	} {
		if code := run(args, &out, &errb); code == 0 {
			t.Errorf("%s: exit 0, want failure", name)
		}
	}
}
