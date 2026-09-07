// Package routing decides where a call destination goes: a local endpoint
// the server controls, the PBX trunk, or nowhere (SPEC §4 "Routing decision").
package routing

import (
	"strings"

	"dialler/server/internal/registry"
	"dialler/server/internal/sip"
)

// Target is the routing outcome.
type Target string

const (
	Local   Target = "local"   // a user in the registry; wake if not registered
	Trunk   Target = "trunk"   // hand off to the PBXAdapter
	Echo    Target = "echo"    // the server answers and plays the caller's audio back (self-test)
	Unknown Target = "unknown" // no local user and no trunk configured
)

// EchoUser is the reserved destination that echoes the caller's audio:
// dial "echo" from the app to test microphone → server → speaker end to
// end with no second party. A provisioned user cannot take this name.
const EchoUser = "echo"

// Decision describes how to reach a destination.
type Decision struct {
	Target     Target
	User       string            // parsed user part
	Endpoint   registry.Endpoint // valid when Target == Local
	Registered bool              // Local only: true → forward INVITE; false → wake first
}

// Router is safe for concurrent use.
type Router struct {
	reg      *registry.Registry
	domains  map[string]bool
	hasTrunk func() bool
}

// New builds a router. localDomains are the SIP domains this server owns;
// a destination with any other domain is never local. hasTrunk reports
// whether a PBX adapter is configured (nil → never).
func New(reg *registry.Registry, localDomains []string, hasTrunk func() bool) *Router {
	d := map[string]bool{}
	for _, x := range localDomains {
		d[strings.ToLower(x)] = true
	}
	if hasTrunk == nil {
		hasTrunk = func() bool { return false }
	}
	return &Router{reg: reg, domains: d, hasTrunk: hasTrunk}
}

// Resolve classifies a destination URI or bare user.
func (r *Router) Resolve(dest string) Decision {
	user := sip.URIUser(dest)
	host := sip.URIHost(dest)
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	d := Decision{Target: Unknown, User: user}
	if user == "" {
		return d
	}
	foreign := host != "" && !r.domains[strings.ToLower(host)]
	if !foreign {
		if strings.EqualFold(user, EchoUser) {
			d.Target = Echo
			return d
		}
		if ep, ok := r.reg.Lookup(user); ok {
			d.Target, d.Endpoint, d.Registered = Local, ep, ep.Contact != ""
			return d
		}
	}
	if r.hasTrunk() {
		d.Target = Trunk
	}
	return d
}
