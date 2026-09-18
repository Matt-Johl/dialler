// Package tlsutil builds the server TLS configuration. In dev a self-signed
// certificate is generated in memory so the server starts with zero setup;
// production supplies a real cert/key pair.
package tlsutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"time"
)

// Load returns a TLS config from certFile/keyFile, or a fresh self-signed
// certificate covering hosts when both paths are empty.
func Load(certFile, keyFile string, hosts []string) (*tls.Config, bool, error) {
	var cert tls.Certificate
	selfSigned := certFile == "" && keyFile == ""
	if selfSigned {
		c, err := SelfSigned(hosts)
		if err != nil {
			return nil, false, err
		}
		cert = c
	} else {
		c, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, false, fmt.Errorf("tlsutil: load key pair: %w", err)
		}
		cert = c
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}, selfSigned, nil
}

// PeerConfig builds the TLS configuration for a SIP trunk to another
// vendor's PBX (SPEC §6 near-term item 3b). It is deliberately NOT Load:
//
//   - TLS 1.2 by default, where the app leg pins 1.3. We own both ends of
//     the app leg and can demand the newer protocol; a PBX we do not own
//     cannot be. CUCM's secure SIP trunks are generally TLS 1.2, so pinning
//     1.3 here would refuse to talk to the exchange this exists for.
//   - The certificate is used for BOTH directions: presented when the PBX
//     calls us (we are the TLS server) and when we call it (CUCM does
//     mutual TLS and expects a client certificate).
//   - caFile is what we verify the PBX against. Empty means the system
//     roots, which is right for a publicly-signed PBX and wrong for the
//     private CA most deployments use — hence the flag.
//
// insecure skips verification entirely: a dev convenience against a
// self-signed PBX, and a hole anywhere else, so the caller warns.
func PeerConfig(certFile, keyFile, caFile string, minVersion uint16, insecure bool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: minVersion, InsecureSkipVerify: insecure} //nolint:gosec // insecure is opt-in and warned about
	if certFile != "" || keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: trunk key pair: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("tlsutil: trunk CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tlsutil: trunk CA %q contains no certificate", caFile)
		}
		// RootCAs verifies the PBX when we dial it; ClientCAs verifies the
		// PBX when it dials us. The same CA signs both in every deployment
		// shape we expect, so set both from the one file.
		cfg.RootCAs = pool
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return cfg, nil
}

// MinTLSVersion maps a flag value ("1.2", "1.3") to the constant.
func MinTLSVersion(s string) (uint16, error) {
	switch strings.TrimSpace(s) {
	case "", "1.2":
		return tls.VersionTLS12, nil
	case "1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("tlsutil: want 1.2 or 1.3; got %q", s)
	}
}

// SelfSigned generates a P-256 certificate valid for one year.
func SelfSigned(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dialler-dev"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
