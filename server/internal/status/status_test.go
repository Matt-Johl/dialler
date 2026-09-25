package status

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dialler/server/internal/admin"
	"dialler/server/internal/b2bua"
	"dialler/server/internal/enroll"
	"dialler/server/internal/gateway"
	"dialler/server/internal/pbxline"
	"dialler/server/internal/registry"
	"dialler/server/internal/wire"
)

func fixture(now func() time.Time) (*Handler, *int) {
	took := 0
	t0 := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	call := b2bua.CallView{CallID: "c1", State: "bridged", Since: t0, A: &b2bua.LegView{Leg: "app", User: "201"}, B: &b2bua.LegView{Leg: "trunk"}}
	deps := Deps{
		Now: now,
		Devices: func() []enroll.Device {
			took++
			return []enroll.Device{
				{DeviceID: "dev-a", User: "201", Description: "A", Enrolled: true},
				{DeviceID: "dev-b", User: "202", Revoked: true, Enrolled: true},
				{DeviceID: "dev-c", User: "203"},
			}
		},
		Sessions: func() map[string][]gateway.SessionInfo {
			return map[string][]gateway.SessionInfo{"dev-a": {
				{Kind: wire.ClientApp, Since: t0, Addr: "10.0.0.5:1", AppVersion: "1.4 (212)"},
				{Kind: wire.ClientExtension, Since: t0, Addr: "10.0.0.5:2"},
			}}
		},
		Endpoints: func() []registry.Endpoint {
			return []registry.Endpoint{{User: "201", DeviceID: "dev-a", Contact: "sip:201@10.0.0.5:1;transport=tls", Expires: t0.Add(time.Hour)}, {User: "202"}, {User: "203"}}
		},
		Lines: func() []pbxline.Status {
			return []pbxline.Status{{User: "201", State: pbxline.StateRegistered, Since: t0, Realm: "asterisk"}, {User: "203", State: pbxline.StateRefused, Error: "403"}}
		},
		Calls:         func() []b2bua.CallView { return []b2bua.CallView{call} },
		Trunk:         func() b2bua.TrunkStatus { return b2bua.TrunkStatus{Configured: true, Qualify: "up", Since: t0} },
		AdminStats:    func() admin.StatsView { return admin.StatsView{InFlight: 1} },
		EventsDropped: func() int64 { return 2 },
	}
	info := Info{Version: "test", StartedAt: t0.Add(-time.Hour), Mode: "lines", Trunk: &TrunkInfo{URI: "sip:asterisk", Transport: "tls"}}
	return New(info, deps), &took
}

func get(h http.Handler, path string, v any) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	if v != nil {
		_ = json.Unmarshal(rec.Body.Bytes(), v)
	}
	return rec.Code
}

func TestStatusMergesTheSources(t *testing.T) {
	now := time.Date(2026, 9, 25, 9, 5, 0, 0, time.UTC)
	h, _ := fixture(func() time.Time { return now })
	var view FleetView
	if code := get(h, "/v1/admin/status", &view); code != 200 {
		t.Fatalf("status: %d", code)
	}
	if view.Trunk.Qualify != "up" || len(view.Devices) != 3 {
		t.Fatalf("%+v", view)
	}
	a := view.Devices[0]
	if a.DeviceID != "dev-a" || a.Sessions["app"] == nil || a.Sessions["app"].AppVersion != "1.4 (212)" || a.Sessions["extension"] == nil {
		t.Fatalf("dev-a sessions: %+v", a.Sessions)
	}
	if a.SIP == nil || !a.SIP.Registered || a.Line == nil || a.Line.State != "registered" || a.Line.Realm != "asterisk" {
		t.Fatalf("dev-a sip/line: %+v %+v", a.SIP, a.Line)
	}
	if a.Call == nil || a.Call.CallID != "c1" {
		t.Fatalf("dev-a call: %+v", a.Call)
	}
	b := view.Devices[1]
	if !b.Revoked || b.SIP != nil || b.Line != nil || b.Call != nil || len(b.Sessions) != 0 {
		t.Fatalf("dev-b (offline, revoked): %+v", b)
	}
	c := view.Devices[2]
	if c.Line == nil || c.Line.State != "refused" || c.Line.Error != "403" || c.Enrolled {
		t.Fatalf("dev-c: %+v", c)
	}
	// Absent fields are omitted, present ones are objects: what a client
	// switches on.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/admin/status", nil))
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	devs := raw["devices"].([]any)
	if _, has := devs[1].(map[string]any)["sip"]; has {
		t.Fatal("sip must be absent when not registered")
	}
	if _, has := devs[0].(map[string]any)["sip"]; !has {
		t.Fatal("sip must be present when registered")
	}
}

func TestServerViewCountsAndUptime(t *testing.T) {
	now := time.Date(2026, 9, 25, 9, 5, 0, 0, time.UTC)
	h, _ := fixture(func() time.Time { return now })
	var view ServerView
	if code := get(h, "/v1/admin/server", &view); code != 200 {
		t.Fatalf("server: %d", code)
	}
	want := Counts{Devices: 3, Enrolled: 2, Revoked: 1, AppOnline: 1, ExtensionOnline: 1, SIPRegistered: 1, LinesRegistered: 1, LinesFailed: 1, Calls: 1}
	if view.Counts != want {
		t.Fatalf("counts %+v, want %+v", view.Counts, want)
	}
	if view.UptimeSeconds != 3900 || view.Version != "test" || view.Go == "" {
		t.Fatalf("identity: uptime %d version %q go %q", view.UptimeSeconds, view.Version, view.Go)
	}
	if view.Trunk == nil || view.Trunk.Qualify != "up" || view.Trunk.URI != "sip:asterisk" {
		t.Fatalf("trunk: %+v", view.Trunk)
	}
	if view.Admin.InFlight != 1 || view.Admin.EventsDropped != 2 {
		t.Fatalf("admin: %+v", view.Admin)
	}
}

func TestCallsAndSnapshotCache(t *testing.T) {
	now := time.Date(2026, 9, 25, 9, 5, 0, 0, time.UTC)
	h, took := fixture(func() time.Time { return now })
	var calls []b2bua.CallView
	if code := get(h, "/v1/admin/calls", &calls); code != 200 || len(calls) != 1 || calls[0].CallID != "c1" {
		t.Fatalf("calls: %d %+v", code, calls)
	}
	// Ten polls within a second take one snapshot; a second later, another.
	for i := 0; i < 10; i++ {
		get(h, "/v1/admin/status", nil)
		get(h, "/v1/admin/server", nil)
	}
	if *took != 1 {
		t.Fatalf("snapshots taken within one second: %d, want 1", *took)
	}
	now = now.Add(admin.SnapshotTTL)
	get(h, "/v1/admin/status", nil)
	if *took != 2 {
		t.Fatalf("snapshots after the TTL: %d, want 2", *took)
	}
}

func TestEmptyDepsAnswerEmptyViews(t *testing.T) {
	h := New(Info{}, Deps{})
	var view FleetView
	if code := get(h, "/v1/admin/status", &view); code != 200 || view.Devices == nil {
		t.Fatalf("status with nothing: %d %+v", code, view)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/admin/calls", nil))
	if rec.Body.String() != "[]\n" {
		t.Fatalf("calls with nothing: %q", rec.Body.String())
	}
}
