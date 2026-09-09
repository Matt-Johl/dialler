package b2bua

import (
	"testing"

	"github.com/emiago/diago/media"

	"dialler/server/internal/pbx"
)

func TestIsTrunkSource(t *testing.T) {
	tr := &pbx.Trunk{Host: "asterisk", Port: 5060, Transport: "tcp"}
	cases := []struct {
		transport, source string
		want              bool
	}{
		{"TCP", "172.19.0.4:5060", true}, // the trunk listener is the only non-TLS one
		{"udp", "10.0.0.9:5060", true},
		{"TLS", "10.18.0.111:53414", false}, // an app
		{"tls", "asterisk:5061", true},      // a TLS trunk, told apart by host
		{"TLS", "ASTERISK:5061", true},
	}
	for _, c := range cases {
		if got := isTrunkSource(c.transport, c.source, tr); got != c.want {
			t.Errorf("%s from %s → %v, want %v", c.transport, c.source, got, c.want)
		}
	}
	if isTrunkSource("udp", "10.0.0.9:5060", nil) {
		t.Error("no trunk configured → never a trunk leg")
	}
}

func TestTrunkBind(t *testing.T) {
	tcp := &pbx.Trunk{Host: "asterisk", Port: 5060, Transport: "tcp"}
	tls := &pbx.Trunk{Host: "pbx", Port: 5061, Transport: "tls"}
	if h, p, err := trunkBind("", tcp); err != nil || h != "0.0.0.0" || p != 5060 {
		t.Errorf("default tcp bind = %s:%d %v", h, p, err)
	}
	if h, p, err := trunkBind("", tls); err != nil || h != "0.0.0.0" || p != 5061 {
		t.Errorf("default tls bind = %s:%d %v", h, p, err)
	}
	if h, p, err := trunkBind(":5062", tcp); err != nil || h != "0.0.0.0" || p != 5062 {
		t.Errorf(":5062 → %s:%d %v", h, p, err)
	}
	if h, p, err := trunkBind("10.0.0.5:5060", tcp); err != nil || h != "10.0.0.5" || p != 5060 {
		t.Errorf("explicit host → %s:%d %v", h, p, err)
	}
	for _, bad := range []string{"nonsense", ":0", ":70000", "10.0.0.5"} {
		if _, _, err := trunkBind(bad, tcp); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestLegNAT(t *testing.T) {
	s := &Server{cfg: Config{NoSymmetricRTP: false}}
	if s.legNAT(false) != media.RTPNATSymetric || s.legNAT(true) != media.RTPNATSymetric {
		t.Error("default: both legs learn the far end's address")
	}
	s = &Server{cfg: Config{NoSymmetricRTP: true}}
	if s.legNAT(false) != media.RTPNATDisabled {
		t.Error("app leg must follow the deployment flag")
	}
	if s.legNAT(true) != media.RTPNATSymetric {
		t.Error("the trunk leg always learns: a PBX behind NAT is normal")
	}
}

func TestTrunkCodecsAreG711Only(t *testing.T) {
	for _, c := range trunkCodecs {
		if c.SampleRate != 8000 || (c.Name != "PCMU" && c.Name != "PCMA") {
			t.Errorf("trunk offered %s/%d; a PBX trunk gets G.711 only", c.Name, c.SampleRate)
		}
	}
}

// The relay does not transcode, so the callee must be constrained to G.711
// whenever EITHER leg is the trunk — otherwise a trunk→app call lets the app
// pick Opus, which the G.711-only trunk caller cannot be answered with, and
// audio dies in both directions (the phone→app-silence bug). app↔app is left
// with its full set.
func TestCallTouchesTrunk(t *testing.T) {
	cases := []struct {
		name string
		l    legs
		want bool
	}{
		{"app to app", legs{}, false},
		{"app to trunk (callee)", legs{calleeTrunk: true}, true},
		{"trunk to app (caller)", legs{callerTrunk: true}, true},
		{"trunk to trunk", legs{callerTrunk: true, calleeTrunk: true}, true},
	}
	for _, c := range cases {
		if got := callTouchesTrunk(c.l); got != c.want {
			t.Errorf("%s: callTouchesTrunk = %v, want %v", c.name, got, c.want)
		}
	}
}
