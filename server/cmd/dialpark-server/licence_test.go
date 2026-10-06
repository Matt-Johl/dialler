package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dialpark/server/internal/enroll"
	"dialpark/server/internal/events"
	"dialpark/server/internal/gateway"
	"dialpark/server/internal/pbxline"
	"dialpark/server/internal/registry"
	"dialpark/server/internal/secrets"
)

// okRegistrar is a PBX that accepts every registration, so a line manager
// can be built in a test without a SIP stack.
type okRegistrar struct{}

func (okRegistrar) Register(context.Context, pbxline.Line, time.Duration) (pbxline.Registration, error) {
	return pbxline.Registration{Expiry: time.Hour}, nil
}
func (okRegistrar) Unregister(context.Context, pbxline.Line) error { return nil }

func lineUsers(m *pbxline.Manager) []string {
	var out []string
	for _, s := range m.Statuses() {
		out = append(out, s.User)
	}
	return out
}

// A device that loses its seat is cut off everywhere the server can reach:
// its gateway sessions, its SIP binding and its PBX line; one that gains a
// seat gets its line back. The registry entry stays, so a call to it is
// answered 480 here rather than routed on to the trunk.
func TestSeatHooksSuspendAndReinstate(t *testing.T) {
	log := slog.Default()
	ring := events.New(64)
	reg := registry.New(nil)
	devices, _ := enroll.Open("")
	devices.Realm = "dialpark"
	box, _ := secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	devices.Secrets = box
	gw := gateway.New(gateway.Config{Logger: log}, devices)
	lines := pbxline.New(pbxline.Config{Registrar: okRegistrar{}, Log: log})
	devices.OnSeats(seatHooks(log, ring, reg, gw, lines, devices))

	devices.SetSeats(3)
	for i, id := range []string{"dev-a", "dev-b", "dev-c"} {
		user := "20" + string(rune('1'+i))
		if _, err := devices.IssueToken(id, user, "fixture-token-"+id); err != nil {
			t.Fatal(err)
		}
		reg.Provision(user, id)
		if _, err := devices.SetPBXLine(id, "", "line"+user, "s"); err != nil {
			t.Fatal(err)
		}
		registerLine(log, lines, devices, id)
	}
	if _, err := reg.Register("203", "sip:203@10.0.0.9:5061;transport=tls", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(lineUsers(lines), ","); got != "201,202,203" {
		t.Fatalf("lines before: %s", got)
	}

	devices.SetSeats(2) // dev-c, the newest, loses its seat
	ep, ok := reg.LookupDevice("dev-c")
	if !ok {
		t.Fatal("seat loss must not deprovision: an inbound call must still resolve locally and be answered 480")
	}
	if ep.Contact != "" {
		t.Fatalf("seat loss must drop the SIP binding, contact still %q", ep.Contact)
	}
	if got := strings.Join(lineUsers(lines), ","); got != "201,202" {
		t.Fatalf("lines after the loss: %s", got)
	}

	devices.SetSeats(3) // and gets it back
	if got := strings.Join(lineUsers(lines), ","); got != "201,202,203" {
		t.Fatalf("lines after the gain: %s", got)
	}

	evs, _, _ := ring.Since(0, 100)
	kinds := map[string][]string{}
	for _, e := range evs {
		kinds[e.Kind] = append(kinds[e.Kind], e.DeviceID)
	}
	if strings.Join(kinds[events.KindSeatLost], ",") != "dev-c" || strings.Join(kinds[events.KindSeatGained], ",") != "dev-a,dev-b,dev-c,dev-c" {
		t.Fatalf("events: lost=%v gained=%v", kinds[events.KindSeatLost], kinds[events.KindSeatGained])
	}
}

// Trunk mode has no line manager; the hooks must still work.
func TestSeatHooksWorkWithoutALineManager(t *testing.T) {
	log := slog.Default()
	ring := events.New(64)
	reg := registry.New(nil)
	devices, _ := enroll.Open("")
	gw := gateway.New(gateway.Config{Logger: log}, devices)
	devices.OnSeats(seatHooks(log, ring, reg, gw, nil, devices))
	devices.SetSeats(1)
	devices.IssueToken("dev-a", "201", "fixture-token-dev-a")
	devices.IssueToken("dev-b", "202", "fixture-token-dev-b") // refused: no seat
	devices.SetSeats(0)
	devices.SetSeats(1)
	evs, _, _ := ring.Since(0, 100)
	n := 0
	for _, e := range evs {
		if e.Kind == events.KindSeatLost || e.Kind == events.KindSeatGained {
			n++
		}
	}
	if n != 3 { // gained, lost, gained
		t.Fatalf("seat events = %d, want 3", n)
	}
}

// The licence is read before any listener: a data directory with no
// licence starts at zero seats and says so; the install id is minted and
// kept; a stored licence for this install sets the count.
func TestStartLicence(t *testing.T) {
	dir := t.TempDir()
	log := slog.Default()
	id, lic, err := startLicence(log, dir)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || lic.Seats() != 0 {
		t.Fatalf("first start: id=%q seats=%d", id, lic.Seats())
	}
	id2, _, err := startLicence(log, dir)
	if err != nil || id2 != id {
		t.Fatalf("second start: id=%q err=%v, want the same id", id2, err)
	}
}
