package main

import (
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dialpark/server/internal/enroll"
	"dialpark/server/internal/pbx"
	"dialpark/server/internal/pbxline"
	"dialpark/server/internal/secrets"
)

func TestParsePBXMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		lines   bool
		wantErr bool
	}{
		{"", false, false}, // the default is the trunk this server has always been
		{"trunk", false, false},
		{"TRUNK", false, false},
		{"lines", true, false},
		{" Lines ", true, false},
		{"both", false, true}, // deliberately not a mode
		{"registered", false, true},
	} {
		lines, err := parsePBXMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parsePBXMode(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if lines != tc.lines {
			t.Errorf("parsePBXMode(%q) = %v, want %v", tc.in, lines, tc.lines)
		}
	}
}

// Lines mode has nothing to register to without a PBX, and saying so at
// start-up is better than a server that comes up and quietly reaches nobody.
func TestLinesModeNeedsATrunk(t *testing.T) {
	_, err := startPBXLines(slog.Default(), options{pbxLines: true}, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "-trunk") {
		t.Errorf("the error should say what is missing, got %v", err)
	}
}

func TestRegistrarURIDefaultsToTheTrunkPeer(t *testing.T) {
	trunk := &pbx.Trunk{Host: "asterisk", Port: 5060, Transport: "tcp"}

	u, err := registrarURI("", trunk)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "asterisk" || u.Port != 5060 {
		t.Errorf("default registrar = %s, want the trunk peer", u.String())
	}
	if tr, _ := u.UriParams.Get("transport"); tr != "tcp" {
		t.Errorf("transport = %q, want the trunk's", tr)
	}

	// A registrar elsewhere: a cluster's publisher, say, while calls come
	// and go on the peer.
	u, err = registrarURI("cucm-pub.example:5061", trunk)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "cucm-pub.example" || u.Port != 5061 {
		t.Errorf("explicit registrar = %s", u.String())
	}

	// Host with no port keeps the trunk's.
	u, err = registrarURI("cucm-pub.example", trunk)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "cucm-pub.example" || u.Port != 5060 {
		t.Errorf("host-only registrar = %s", u.String())
	}

	// A sip: prefix is what an operator will paste.
	if u, err = registrarURI("sip:cucm-pub.example", trunk); err != nil || u.Host != "cucm-pub.example" {
		t.Errorf("sip: prefix = %s %v", u.String(), err)
	}

	for _, bad := range []string{"cucm:0", "cucm:99999", "cucm:abc", ":"} {
		if _, err := registrarURI(bad, trunk); err == nil {
			t.Errorf("registrarURI(%q) should have failed", bad)
		}
	}
}

func TestSplitList(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ,, ", []string{"a", "b"}},
	} {
		if got := splitList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitList(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// registerLine registers a device's stored line unless the device is
// enrolled and holds no seat (SPEC §4.9). An un-enrolled device with a line
// is registered, as it always was: the line answers once the phone arrives.
func TestRegisterLineHonoursSeats(t *testing.T) {
	log := slog.Default()
	devices, _ := enroll.Open("")
	devices.Realm = "dialpark"
	box, _ := secrets.OpenKey(filepath.Join(t.TempDir(), "pbx.key"))
	devices.Secrets = box
	mgr := pbxline.New(pbxline.Config{Registrar: okRegistrar{}, Log: log})

	devices.SetSeats(2)
	devices.IssueToken("dev-a", "201", "fixture-token-dev-a")
	devices.IssueToken("dev-c", "203", "fixture-token-dev-c")
	devices.Create("dev-b", "202", "staged")
	for _, id := range []string{"dev-a", "dev-b", "dev-c"} {
		if _, err := devices.SetPBXLine(id, "", "line-"+id, "s"); err != nil {
			t.Fatal(err)
		}
	}
	devices.SetSeats(1) // dev-c is now beyond the line
	for _, id := range []string{"dev-a", "dev-b", "dev-c"} {
		registerLine(log, mgr, devices, id)
	}
	if got := strings.Join(lineUsers(mgr), ","); got != "201,202" {
		t.Fatalf("registered lines = %s, want the holder and the un-enrolled device, not the unlicensed one", got)
	}
	// No line stored: nothing to do, no panic.
	devices.Create("dev-d", "204", "")
	registerLine(log, mgr, devices, "dev-d")
	// Trunk mode: no manager, no panic.
	registerLine(log, nil, devices, "dev-a")
}
