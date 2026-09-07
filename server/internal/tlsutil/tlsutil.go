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
