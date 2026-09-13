// Package qos marks the server's sockets for quality of service: DSCP EF on
// media, CS3 on signalling (SPEC §4.4). A socket option set on a listener is
// inherited by the connections it accepts, so marking the listeners covers
// every TLS session; the RTP sockets are marked as they are opened.
//
// Standard library only: the option is applied through the ListenConfig
// Control hook on the raw descriptor. Both the IPv4 and the IPv6 option are
// attempted; the one that does not apply to the socket's family fails with
// ENOPROTOOPT/EINVAL and is ignored.
package qos

import (
	"errors"
	"syscall"
)

// DSCP code points used here (RFC 4594): EF for voice media, CS3 for
// call signalling.
const (
	DSCPEF  = 46
	DSCPCS3 = 24
)

// TOS returns the IP TOS byte carrying a DSCP code point.
func TOS(dscp int) int { return dscp << 2 }

// Control returns a net.ListenConfig / net.Dialer Control function that
// marks the socket with dscp.
func Control(dscp int) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var err error
		cerr := c.Control(func(fd uintptr) { err = Mark(int(fd), dscp) })
		if cerr != nil {
			return cerr
		}
		return err
	}
}

// Mark sets the DSCP on an open socket descriptor.
func Mark(fd int, dscp int) error {
	tos := TOS(dscp)
	e4 := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_TOS, tos)
	e6 := syscall.SetsockoptInt(fd, syscall.IPPROTO_IPV6, syscall.IPV6_TCLASS, tos)
	if e4 == nil || e6 == nil {
		return nil
	}
	if ignorable(e4) && ignorable(e6) {
		return nil
	}
	if !ignorable(e4) {
		return e4
	}
	return e6
}

func ignorable(err error) bool {
	return errors.Is(err, syscall.ENOPROTOOPT) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.EOPNOTSUPP)
}
