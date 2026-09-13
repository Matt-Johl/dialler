package main

import (
	"net"
	"testing"
)

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
