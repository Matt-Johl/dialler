// Package pbx defines the PBXAdapter seam (SPEC §4.4 rule 7): the only
// thing the light server knows about a PBX is how to reach it as a SIP
// trunk peer. AMI/ESL adapters, if ever written, live behind this interface
// and are dev-only.
package pbx

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Trunk describes the SIP peer the server sends non-local calls to.
type Trunk struct {
	Host      string
	Port      int
	Transport string // "udp" | "tcp" | "tls"; the app leg is TLS-only but the trunk leg follows the PBX
	// Registration, when the PBX requires the server to REGISTER as a
	// third-party device (CUCM). Empty means the PBX trusts the server by IP.
	RegisterUser string
	RegisterPass string
}

// ErrBadTrunk is returned by ParseTrunk for anything that is not a SIP peer address.
var ErrBadTrunk = errors.New("pbx: trunk must be host[:port][;transport=udp|tcp|tls], optionally prefixed with sip:")

// ParseTrunk reads a trunk peer from its configuration form:
// "asterisk", "asterisk:5060", "sip:asterisk:5060;transport=tcp",
// "10.0.0.5;transport=udp". Port defaults to 5060 (5061 for tls);
// transport defaults to udp, the SIP default.
func ParseTrunk(s string) (Trunk, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "sip:")
	if s == "" {
		return Trunk{}, ErrBadTrunk
	}
	t := Trunk{Transport: "udp"}
	parts := strings.Split(s, ";")
	hostport := parts[0]
	for _, p := range parts[1:] {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) == 2 && strings.EqualFold(kv[0], "transport") {
			t.Transport = strings.ToLower(kv[1])
		}
	}
	switch t.Transport {
	case "udp", "tcp", "tls":
	default:
		return Trunk{}, ErrBadTrunk
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		// no port
		host = strings.Trim(hostport, "[]")
		port = ""
	}
	if host == "" {
		return Trunk{}, ErrBadTrunk
	}
	t.Host = host
	if port == "" {
		t.Port = 5060
		if t.Transport == "tls" {
			t.Port = 5061
		}
	} else {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return Trunk{}, ErrBadTrunk
		}
		t.Port = n
	}
	return t, nil
}

// Address is "host:port".
func (t Trunk) Address() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// URI builds the request URI for user at the trunk, e.g.
// "sip:100@asterisk:5060;transport=tcp".
func (t Trunk) URI(user string) string {
	return fmt.Sprintf("sip:%s@%s;transport=%s", user, t.Address(), t.Transport)
}

// Adapter is implemented per PBX flavour. The Phase 0 skeleton ships only
// None; a generic SIP-trunk adapter arrives with the B2BUA.
type Adapter interface {
	Name() string
	Trunk() (Trunk, bool) // false → no trunk configured
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// None is the adapter used when no PBX is configured (standalone mode).
type None struct{}

func (None) Name() string                { return "none" }
func (None) Trunk() (Trunk, bool)        { return Trunk{}, false }
func (None) Start(context.Context) error { return nil }
func (None) Stop(context.Context) error  { return nil }

// SIPTrunk is the generic adapter: the PBX is a SIP peer trusted by
// address (Asterisk `identify`, CUCM trunk). Nothing runs in the
// background yet; third-party registration (CUCM) is a later addition
// behind the same interface.
type SIPTrunk struct{ trunk Trunk }

// NewSIPTrunk wraps a parsed trunk.
func NewSIPTrunk(t Trunk) *SIPTrunk { return &SIPTrunk{trunk: t} }

func (s *SIPTrunk) Name() string                { return "sip-trunk" }
func (s *SIPTrunk) Trunk() (Trunk, bool)        { return s.trunk, true }
func (s *SIPTrunk) Start(context.Context) error { return nil }
func (s *SIPTrunk) Stop(context.Context) error  { return nil }
