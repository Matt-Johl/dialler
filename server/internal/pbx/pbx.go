// Package pbx defines the PBXAdapter seam (SPEC §4.4 rule 7): the only
// thing the light server knows about a PBX is how to reach it as a SIP
// trunk peer. AMI/ESL adapters, if ever written, live behind this interface
// and are dev-only.
package pbx

import "context"

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
