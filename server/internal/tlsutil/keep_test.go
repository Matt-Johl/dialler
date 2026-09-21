package tlsutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The dev certificate must survive a restart: apps pin its fingerprint.
func TestLoadOrKeepReusesTheSelfSignedCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	first, selfSigned, err := LoadOrKeep("", "", []string{"10.0.0.1"}, dir)
	if err != nil || !selfSigned {
		t.Fatalf("first: %v selfSigned=%v", err, selfSigned)
	}
	for _, name := range []string{"self-signed.pem", "self-signed.key"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s not written: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", name, info.Mode().Perm())
		}
	}
	second, _, err := LoadOrKeep("", "", []string{"10.0.0.2"}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Certificates[0].Certificate[0], second.Certificates[0].Certificate[0]) {
		t.Fatal("a second start must present the same certificate")
	}
	other, _, _ := LoadOrKeep("", "", []string{"10.0.0.1"}, filepath.Join(t.TempDir(), "tls"))
	if bytes.Equal(first.Certificates[0].Certificate[0], other.Certificates[0].Certificate[0]) {
		t.Fatal("a different cache directory must not share the certificate")
	}
	// No cache directory: the old in-memory behaviour.
	fresh, _, _ := LoadOrKeep("", "", []string{"10.0.0.1"}, "")
	if bytes.Equal(first.Certificates[0].Certificate[0], fresh.Certificates[0].Certificate[0]) {
		t.Fatal("no cacheDir must generate afresh")
	}
}
