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
		if got := isTrunkSource(c.transport, c.source, tr, nil); got != c.want {
			t.Errorf("%s from %s → %v, want %v", c.transport, c.source, got, c.want)
		}
	}
	if isTrunkSource("udp", "10.0.0.9:5060", nil, nil) {
		t.Error("no trunk configured → never a trunk leg")
	}
}

// A cluster originates from whichever node handles the call, not only the
// one we send to. An unnamed node's INVITE is challenged like an app's and
// the call fails, so every node has to be trustable (-pbx-peers).
func TestIsTrunkSourceAcceptsTheOtherClusterNodes(t *testing.T) {
	tr := &pbx.Trunk{Host: "cucm-pub", Port: 5061, Transport: "tls"}
	peers := []string{"cucm-sub1", "cucm-sub2", ""}
	for _, c := range []struct {
		source string
		want   bool
	}{
		{"cucm-pub:5061", true},
		{"cucm-sub1:5061", true},
		{"CUCM-SUB2:5061", true},
		{"10.18.0.111:53414", false}, // an app, still challenged
		{"cucm-sub3:5061", false},    // a node nobody named
	} {
		if got := isTrunkSource("tls", c.source, tr, peers); got != c.want {
			t.Errorf("tls from %s → %v, want %v", c.source, got, c.want)
		}
	}
}

func TestTrunkBind(t *testing.T) {
	tcp := &pbx.Trunk{Host: "asterisk", Port: 5060, Transport: "tcp"}
	tls := &pbx.Trunk{Host: "pbx", Port: 5061, Transport: "tls"}
	if h, p, err := trunkBind("", tcp); err != nil || h != "0.0.0.0" || p != 5060 {
		t.Errorf("default tcp bind = %s:%d %v", h, p, err)
	}
	// 5062, not the conventional 5061: that is the app leg's port and the
	// two transports cannot share a socket.
	if h, p, err := trunkBind("", tls); err != nil || h != "0.0.0.0" || p != 5062 {
		t.Errorf("default tls bind = %s:%d %v", h, p, err)
	}
	if h, p, err := trunkBind("", &pbx.Trunk{Host: "pbx", Port: 5061, Transport: "TLS"}); err != nil || p != 5062 {
		t.Errorf("uppercase transport → %s:%d %v", h, p, err)
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

// A PBX trunk is offered the G.711 family the relay can copy end to end
// without transcoding: G.722 wideband first (RTP clock 8000 like G.711,
// RFC 3551), then PCMU/PCMA. Never Opus by default — Asterisk here has no
// Opus and CUCM trunks do not either.
func TestTrunkCodecsAreWidebandThenG711(t *testing.T) {
	codecs := Config{}.trunkCodecs()
	if len(codecs) == 0 || codecs[0].Name != "G722" {
		t.Fatalf("default trunk offer = %v; G.722 must come first", codecs)
	}
	for _, c := range codecs {
		if c.SampleRate != 8000 || (c.Name != "G722" && c.Name != "PCMU" && c.Name != "PCMA") {
			t.Errorf("trunk offered %s/%d; a PBX trunk gets G.722 or G.711 only", c.Name, c.SampleRate)
		}
	}
	narrow := Config{TrunkCodecs: []media.Codec{media.CodecAudioUlaw}}.trunkCodecs()
	if len(narrow) != 1 || narrow[0].Name != "PCMU" {
		t.Errorf("configured trunk codecs not honoured: %v", narrow)
	}
}

func TestParseCodecs(t *testing.T) {
	got, err := ParseCodecs("g722, PCMU ,alaw")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "G722" || got[1].Name != "PCMU" || got[2].Name != "PCMA" {
		t.Errorf("ParseCodecs = %v", got)
	}
	if _, err := ParseCodecs("g722,g729"); err == nil {
		t.Error("an unknown codec name must be refused, not ignored")
	}
	if _, err := ParseCodecs(""); err == nil {
		t.Error("an empty list must be refused")
	}
}

// The vendored SDP reader must recognise G.722 both as a bare static
// payload type and with its rtpmap, and must not confuse it with G.711.
func TestSDPReadsG722(t *testing.T) {
	var codecs [8]media.Codec
	n, err := media.CodecsFromSDPRead([]string{"9", "0", "8"}, nil, codecs[:])
	if err != nil || n != 3 || codecs[0].Name != "G722" || codecs[0].PayloadType != 9 || codecs[0].SampleRate != 8000 {
		t.Fatalf("static G722 offer: n=%d err=%v codecs=%v", n, err, codecs[:n])
	}
	n, err = media.CodecsFromSDPRead([]string{"96", "9"}, []string{"rtpmap:96 opus/48000/2", "rtpmap:9 G722/8000"}, codecs[:])
	if err != nil || n != 2 || codecs[1] != media.CodecAudioG722 {
		t.Fatalf("G722 with rtpmap: n=%d err=%v codecs=%v", n, err, codecs[:n])
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
