package main

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"dialler/server/internal/pbx"
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
	_, err := startPBXLines(slog.Default(), options{pbxLines: true}, nil, nil, nil)
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
