package pbx

import "testing"

func TestParseTrunk(t *testing.T) {
	cases := []struct {
		in   string
		want Trunk
	}{
		{"asterisk", Trunk{Host: "asterisk", Port: 5060, Transport: "udp"}},
		{"asterisk:5062", Trunk{Host: "asterisk", Port: 5062, Transport: "udp"}},
		{"sip:asterisk:5060;transport=tcp", Trunk{Host: "asterisk", Port: 5060, Transport: "tcp"}},
		{"10.0.0.5;transport=TCP", Trunk{Host: "10.0.0.5", Port: 5060, Transport: "tcp"}},
		{"pbx.example.com;transport=tls", Trunk{Host: "pbx.example.com", Port: 5061, Transport: "tls"}},
		{" sip:[fd00::5]:5060;transport=udp ", Trunk{Host: "fd00::5", Port: 5060, Transport: "udp"}},
	}
	for _, c := range cases {
		got, err := ParseTrunk(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q → %+v, want %+v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "sip:", ";transport=tcp", "asterisk;transport=sctp", "asterisk:0", "asterisk:99999"} {
		if _, err := ParseTrunk(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestTrunkURI(t *testing.T) {
	tr := Trunk{Host: "asterisk", Port: 5060, Transport: "tcp"}
	if got := tr.URI("100"); got != "sip:100@asterisk:5060;transport=tcp" {
		t.Errorf("URI = %s", got)
	}
	if got := tr.Address(); got != "asterisk:5060" {
		t.Errorf("Address = %s", got)
	}
	a := NewSIPTrunk(tr)
	if got, ok := a.Trunk(); !ok || got != tr {
		t.Errorf("adapter Trunk() = %+v %v", got, ok)
	}
	if _, ok := (None{}).Trunk(); ok {
		t.Error("None must report no trunk")
	}
}
