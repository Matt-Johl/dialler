package routing

import (
	"testing"
	"time"

	"dialler/server/internal/registry"
)

func TestResolve(t *testing.T) {
	reg := registry.New(nil)
	reg.Provision("201", "dev1")
	reg.Provision("202", "dev2")
	_, _ = reg.Register("202", "<sip:202@10.0.0.6:5061;transport=tls>", time.Minute)

	trunk := false
	r := New(reg, []string{"dialler.example.local"}, func() bool { return trunk })

	cases := []struct {
		dest       string
		target     Target
		registered bool
	}{
		{"sip:201@dialler.example.local", Local, false},
		{"<sip:202@dialler.example.local:5061>", Local, true},
		{"201", Local, false},
		{"sip:201@DIALLER.example.local", Local, false},
		{"sip:201@pbx.example.local", Unknown, false}, // foreign domain never local
		{"sip:0123456789@dialler.example.local", Unknown, false},
		{"sip:dialler.example.local", Unknown, false}, // no user part
	}
	for _, c := range cases {
		d := r.Resolve(c.dest)
		if d.Target != c.target || d.Registered != c.registered {
			t.Errorf("%s → %+v, want %s registered=%v", c.dest, d, c.target, c.registered)
		}
	}

	trunk = true
	if d := r.Resolve("sip:0123456789@dialler.example.local"); d.Target != Trunk {
		t.Errorf("with trunk, external number → %s", d.Target)
	}
	if d := r.Resolve("sip:201@pbx.example.local"); d.Target != Trunk {
		t.Errorf("with trunk, foreign domain → %s", d.Target)
	}
	if d := r.Resolve("sip:201@dialler.example.local"); d.Target != Local {
		t.Errorf("trunk must not shadow a local user: %s", d.Target)
	}
}
