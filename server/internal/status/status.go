// Package status serves the operator's read-only view of the call server
// (SPEC §4.8): what it is, and for every device whether it is enrolled,
// connected and registered. Nothing here changes anything; the admin UI
// polls it.
package status

import (
	"encoding/json"
	"net/http"
	"time"

	"dialler/server/internal/enroll"
	"dialler/server/internal/registry"
	"dialler/server/internal/wire"
)

// Server describes the call server itself.
type Server struct {
	PublicHost string    `json:"public_host"`
	SIPDomain  string    `json:"sip_domain"`
	SignalPort int       `json:"signal_port"`
	SIPPort    int       `json:"sip_port"`
	HTTPSPort  int       `json:"https_port"`
	CertSHA256 string    `json:"cert_sha256"`
	StartedAt  time.Time `json:"started_at"`
	// Trunk is the PBX peer, "" when the server runs standalone.
	Trunk string `json:"trunk,omitempty"`
}

// Device is one device with its live state folded in.
type Device struct {
	enroll.Device
	// Online lists the connection kinds the device holds now.
	Online []wire.ClientKind `json:"online"`
	// Registered is whether the device's user holds a live SIP registration.
	Registered bool `json:"registered"`
	// RegistrationExpires is when that registration lapses; omitted otherwise.
	RegistrationExpires *time.Time `json:"registration_expires,omitempty"`
}

// Response is the body of GET /v1/admin/status.
type Response struct {
	Server  Server    `json:"server"`
	Devices []Device  `json:"devices"`
	Now     time.Time `json:"now"`
}

// Sources are the live tables the status is read from.
type Sources struct {
	Devices  func() []enroll.Device
	Sessions func(deviceID string) []wire.ClientKind
	Lookup   func(user string) (registry.Endpoint, bool)
	Now      func() time.Time
}

// Handler serves GET /v1/admin/status behind the admin bearer token.
func Handler(server Server, adminToken string, src Sources) http.Handler {
	if src.Now == nil {
		src.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/status", func(w http.ResponseWriter, r *http.Request) {
		if adminToken == "" || !enroll.BearerMatches(r, adminToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		now := src.Now()
		resp := Response{Server: server, Now: now, Devices: []Device{}}
		for _, d := range src.Devices() {
			dev := Device{Device: d, Online: src.Sessions(d.DeviceID)}
			if dev.Online == nil {
				dev.Online = []wire.ClientKind{}
			}
			if ep, ok := src.Lookup(d.User); ok && !ep.Expires.IsZero() && ep.Expires.After(now) && ep.DeviceID == d.DeviceID {
				dev.Registered = true
				exp := ep.Expires
				dev.RegistrationExpires = &exp
			}
			resp.Devices = append(resp.Devices, dev)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}
