package main

import (
	"errors"
	"net"
	"testing"
)

func TestResolveAdminToken(t *testing.T) {
	files := map[string]string{"tok": "  s3cret\n", "empty": " \n"}
	read := func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
	if got, err := resolveAdminToken("lit", "", read); err != nil || got != "lit" {
		t.Fatalf("literal: %q %v", got, err)
	}
	if got, err := resolveAdminToken("", "", read); err != nil || got != "" {
		t.Fatalf("neither: %q %v (empty means generate)", got, err)
	}
	if got, err := resolveAdminToken("", "tok", read); err != nil || got != "s3cret" {
		t.Fatalf("file: %q %v (must be trimmed)", got, err)
	}
	if _, err := resolveAdminToken("", "empty", read); err == nil {
		t.Fatal("an empty file must be refused, not become an empty token")
	}
	if _, err := resolveAdminToken("", "missing", read); err == nil {
		t.Fatal("a missing file must be refused")
	}
	if _, err := resolveAdminToken("lit", "tok", read); err == nil {
		t.Fatal("both flags at once must be refused")
	}
}

func TestCheckPublicHost(t *testing.T) {
	addrs := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("10.18.0.212"), Mask: net.CIDRMask(24, 32)},
		}, nil
	}
	for _, tc := range []struct {
		host string
		ok   bool
	}{
		{"10.18.0.212", true},
		{"127.0.0.1", true},
		{"10.18.0.168", false}, // the stale-DHCP case: not ours, apps would time out
		{"dialler", true},      // hostnames are left to DNS
		{"pbx.example.com", true},
	} {
		err := checkPublicHost(tc.host, addrs)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.host, err, tc.ok)
		}
	}
	if err := checkPublicHost("10.18.0.168", func() ([]net.Addr, error) { return nil, net.ErrClosed }); err != nil {
		t.Errorf("an unreadable interface table must not block startup: %v", err)
	}
}
