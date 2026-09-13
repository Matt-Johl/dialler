package qos

import (
	"syscall"
	"testing"
)

// A raw descriptor stands in for the RawConn: no bind, so it runs in the
// sandbox too.
type fakeRawConn struct{ fd int }

func (f fakeRawConn) Control(fn func(fd uintptr)) error { fn(uintptr(f.fd)); return nil }
func (f fakeRawConn) Read(func(fd uintptr) bool) error  { return nil }
func (f fakeRawConn) Write(func(fd uintptr) bool) error { return nil }

func TestControlMarksTheSocket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		domain int
		dscp   int
		opt    func(fd int) (int, error)
	}{
		{"udp4 EF", syscall.AF_INET, DSCPEF, func(fd int) (int, error) { return syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TOS) }},
		{"tcp4 CS3", syscall.AF_INET, DSCPCS3, func(fd int) (int, error) { return syscall.GetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TOS) }},
		{"udp6 EF", syscall.AF_INET6, DSCPEF, func(fd int) (int, error) { return syscall.GetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_TCLASS) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typ := syscall.SOCK_DGRAM
			if tc.name[:3] == "tcp" {
				typ = syscall.SOCK_STREAM
			}
			fd, err := syscall.Socket(tc.domain, typ, 0)
			if err != nil {
				t.Skipf("socket(): %v", err)
			}
			defer syscall.Close(fd)
			if err := Control(tc.dscp)("", "", fakeRawConn{fd}); err != nil {
				t.Fatalf("Control: %v", err)
			}
			got, err := tc.opt(fd)
			if err != nil {
				t.Fatalf("getsockopt: %v", err)
			}
			if got != TOS(tc.dscp) {
				t.Fatalf("tos = 0x%x, want 0x%x", got, TOS(tc.dscp))
			}
		})
	}
	if TOS(DSCPEF) != 0xb8 || TOS(DSCPCS3) != 0x60 {
		t.Fatal("EF must be tos 0xb8 and CS3 0x60")
	}
}
