package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMinTLSVersion(t *testing.T) {
	// Empty must mean 1.2, not 0: a zero MinVersion lets crypto/tls fall
	// back to its own floor rather than the one the trunk documents.
	for _, c := range []struct {
		in   string
		want uint16
	}{{"", tls.VersionTLS12}, {"1.2", tls.VersionTLS12}, {" 1.2 ", tls.VersionTLS12}, {"1.3", tls.VersionTLS13}} {
		got, err := MinTLSVersion(c.in)
		if err != nil || got != c.want {
			t.Errorf("MinTLSVersion(%q) = %x, %v; want %x", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"1.1", "TLSv1.2", "1.4", "12"} {
		if _, err := MinTLSVersion(bad); err == nil {
			t.Errorf("MinTLSVersion(%q): expected an error", bad)
		}
	}
}

func TestPeerConfigBare(t *testing.T) {
	cfg, err := PeerConfig("", "", "", tls.VersionTLS12, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS12 || cfg.InsecureSkipVerify || cfg.RootCAs != nil || len(cfg.Certificates) != 0 {
		t.Errorf("bare config = %+v", cfg)
	}
}

func TestPeerConfigCertAndCA(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "pbx")

	cfg, err := PeerConfig(certPath, keyPath, certPath, tls.VersionTLS13, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("certificates = %d, want 1", len(cfg.Certificates))
	}
	if cfg.MinVersion != tls.VersionTLS13 || !cfg.InsecureSkipVerify {
		t.Errorf("min=%x insecure=%v", cfg.MinVersion, cfg.InsecureSkipVerify)
	}
	// Both directions come off the one CA file: RootCAs verifies the PBX
	// when we dial it, ClientCAs when it dials us. A config with only one
	// set works for exactly half the calls, which is the failure this
	// asserts against.
	if cfg.RootCAs == nil || cfg.ClientCAs == nil {
		t.Errorf("RootCAs=%v ClientCAs=%v, want both", cfg.RootCAs != nil, cfg.ClientCAs != nil)
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v", cfg.ClientAuth)
	}
}

func TestPeerConfigErrors(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "pbx")

	if _, err := PeerConfig(filepath.Join(dir, "absent.pem"), keyPath, "", tls.VersionTLS12, false); err == nil {
		t.Error("missing certificate: expected an error")
	}
	if _, err := PeerConfig(certPath, keyPath, filepath.Join(dir, "absent-ca.pem"), tls.VersionTLS12, false); err == nil {
		t.Error("missing CA: expected an error")
	}
	// A file that is not a certificate must fail here rather than at the
	// first call, when an empty pool silently rejects the PBX.
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PeerConfig("", "", junk, tls.VersionTLS12, false); err == nil {
		t.Error("CA with no certificate in it: expected an error")
	}
}

// TestPeerConfigHandshake proves the config is usable in both roles: a
// server built from it accepts a client built from it, with each verifying
// the other against the shared CA.
func TestPeerConfigHandshake(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, "127.0.0.1")

	server, err := PeerConfig(certPath, keyPath, certPath, tls.VersionTLS12, false)
	if err != nil {
		t.Fatal(err)
	}
	client, err := PeerConfig(certPath, keyPath, certPath, tls.VersionTLS12, false)
	if err != nil {
		t.Fatal(err)
	}
	client.ServerName = "127.0.0.1"

	// No listener: run the handshake over an in-memory pipe, which needs no
	// socket and so works wherever the tests do.
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	sc := tls.Server(s, server)
	cc := tls.Client(c, client)
	errc := make(chan error, 1)
	go func() { errc <- sc.Handshake() }()
	if err := cc.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	if st := sc.ConnectionState(); len(st.PeerCertificates) == 0 {
		t.Error("server saw no client certificate; CUCM's mutual TLS would reject us")
	}
}

// writeSelfSigned writes a certificate valid for host and its key, and
// returns the two paths. It is its own CA and carries both ServerAuth and
// ClientAuth, so the one file stands in for all three trunk flags — the
// shape a small private CA produces, and what the handshake test needs from
// both ends.
func writeSelfSigned(t *testing.T, dir, host string) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}
