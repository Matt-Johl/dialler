package status

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"dialler/server/internal/enroll"
	"dialler/server/internal/registry"
	"dialler/server/internal/wire"
)

func TestStatusFoldsLiveStateIntoEachDevice(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	src := Sources{
		Devices: func() []enroll.Device {
			return []enroll.Device{
				{DeviceID: "dev-a", User: "201", Label: "Matt's iPhone", Enrolled: true},
				{DeviceID: "dev-b", User: "202", Enrolled: false},
				{DeviceID: "dev-c", User: "203", Enrolled: true, Revoked: true},
			}
		},
		Sessions: func(id string) []wire.ClientKind {
			if id == "dev-a" {
				return []wire.ClientKind{wire.ClientApp, wire.ClientExtension}
			}
			return nil
		},
		Lookup: func(user string) (registry.Endpoint, bool) {
			switch user {
			case "201":
				return registry.Endpoint{User: "201", DeviceID: "dev-a", Contact: "sip:201@10.0.0.4", Expires: now.Add(time.Hour)}, true
			case "203": // a registration that has lapsed
				return registry.Endpoint{User: "203", DeviceID: "dev-c", Expires: now.Add(-time.Minute)}, true
			}
			return registry.Endpoint{}, false
		},
		Now: func() time.Time { return now },
	}
	h := Handler(Server{PublicHost: "10.0.0.1", SIPDomain: "dialler", SignalPort: 7443, SIPPort: 5061, HTTPSPort: 8080, CertSHA256: "fp", StartedAt: now.Add(-time.Hour)}, "admin", src)

	req := httptest.NewRequest("GET", "/v1/admin/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	req.Header.Set("Authorization", "Bearer admin")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Server.SIPDomain != "dialler" || resp.Server.CertSHA256 != "fp" || !resp.Now.Equal(now) {
		t.Fatalf("server: %+v", resp.Server)
	}
	if len(resp.Devices) != 3 {
		t.Fatalf("devices: %d", len(resp.Devices))
	}
	a, b, c := resp.Devices[0], resp.Devices[1], resp.Devices[2]
	if a.Label != "Matt's iPhone" || len(a.Online) != 2 || !a.Registered || a.RegistrationExpires == nil {
		t.Fatalf("dev-a: %+v", a)
	}
	if len(b.Online) != 0 || b.Registered || b.RegistrationExpires != nil {
		t.Fatalf("dev-b: %+v", b)
	}
	if c.Registered {
		t.Fatalf("dev-c's registration has lapsed: %+v", c)
	}
	// The JSON shape the UI reads: "online" is always an array.
	if !json.Valid(rec.Body.Bytes()) || !contains(rec.Body.String(), `"online":[]`) {
		t.Fatalf("body: %s", rec.Body.String())
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
