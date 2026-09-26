package main

import (
	"log/slog"
	"path/filepath"
	"testing"

	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/events"
	"dialler/server/internal/gateway"
	"dialler/server/internal/registry"
)

// The hooks the admin API fires must all work in TRUNK mode, where there
// is no line manager: an operator may provision a PBX line ahead of a
// switch to lines mode, and revoking or purging a device must not need
// one either. Found by the client-states harness on 2026-09-26: setting a
// line in trunk mode panicked the handler.
func TestAdminHooksWorkWithoutALineManager(t *testing.T) {
	log := slog.Default()
	ring := events.New(64)
	reg := registry.New(nil)
	devices, _ := enroll.Open("")
	dir, _ := directory.Open("")
	gw := gateway.New(gateway.Config{Logger: log}, devices)
	hooks := adminHooks(log, ring, reg, gw, nil, devices, dir, filepath.Join(t.TempDir(), "diag"))

	devices.Create("dev-a", "201", "A")
	hooks.OnIssue("dev-a", "201")
	if _, ok := reg.LookupDevice("dev-a"); !ok {
		t.Fatal("OnIssue did not provision the registry")
	}
	// A line set or removed with no registrar to tell: stored, event
	// emitted, no panic.
	hooks.OnPBXLine("dev-a", &enroll.PBXCredential{DeviceID: "dev-a", User: "201", DN: "201", DigestUser: "line201", Secret: "s"})
	hooks.OnPBXLine("dev-a", nil)
	hooks.OnConfig("dev-a", enroll.DeviceConfig{Version: 1, SSIDs: []string{"Office"}})
	hooks.OnClaim("dev-a", "201")
	hooks.OnRevoke("dev-a")
	if _, ok := reg.LookupDevice("dev-a"); ok {
		t.Fatal("OnRevoke left the registry entry")
	}
	hooks.OnPurge("dev-a")

	evs, _, _ := ring.Since(0, 100)
	kinds := map[string]int{}
	for _, e := range evs {
		kinds[e.Kind]++
	}
	for _, want := range []string{events.KindLineChanged, events.KindLineRemoved, events.KindConfigChanged, events.KindCodeClaimed, events.KindDeviceRevoked, events.KindDevicePurged} {
		if kinds[want] == 0 {
			t.Fatalf("no %s event; got %v", want, kinds)
		}
	}
}
