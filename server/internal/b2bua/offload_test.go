package b2bua

import (
	"errors"
	"fmt"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"dialler/server/internal/routing"
)

// A transfer is handed to the PBX only when this server would otherwise be
// relaying between two PBX parties for no reason: the remaining party is on
// the trunk and the target is a PBX destination. Anything touching an app
// leg (which always relays through us) or the server's own echo stays here.
func TestShouldOffloadOnlyWhenBothPartiesAreOnThePBX(t *testing.T) {
	phone := &callLeg{name: "phone", trunk: true}
	app := &callLeg{name: "app"}
	cases := []struct {
		name      string
		remaining *callLeg
		target    routing.Target
		want      bool
	}{
		{"phone stays, target is a PBX extension", phone, routing.Trunk, true},
		{"phone stays, target is an app user", phone, routing.Local, false},
		{"phone stays, target is the server's echo", phone, routing.Echo, false},
		{"app stays, target is a PBX extension", app, routing.Trunk, false},
		{"app stays, target is an app user", app, routing.Local, false},
		{"nobody stays (echo call)", nil, routing.Trunk, false},
	}
	for _, c := range cases {
		if got := shouldOffload(c.remaining, c.target); got != c.want {
			t.Errorf("%s: shouldOffload = %v, want %v", c.name, got, c.want)
		}
	}
}

// What the PBX's NOTIFY about our REFER means: 1xx keeps waiting, 2xx is
// done, anything else means the PBX did not complete it and we fall back.
func TestReferOutcome(t *testing.T) {
	cases := []struct {
		code           int
		final, success bool
	}{
		{100, false, false},
		{180, false, false},
		{200, true, true},
		{202, true, true},
		{404, true, false},
		{486, true, false},
		{503, true, false},
		{603, true, false},
	}
	for _, c := range cases {
		final, ok := referOutcome(c.code)
		if final != c.final || ok != c.success {
			t.Errorf("NOTIFY %d: final=%v success=%v, want %v/%v", c.code, final, ok, c.final, c.success)
		}
	}
}

// The status inside a rejected REFER is what gets logged; a non-SIP error
// yields 0 and a generic reason.
func TestResponseStatus(t *testing.T) {
	res := &sip.Response{StatusCode: 405, Reason: "Method Not Allowed"}
	if got := responseStatus(sipgo.ErrDialogResponse{Res: res}); got != 405 {
		t.Errorf("by value: %d", got)
	}
	if got := responseStatus(fmt.Errorf("refer: %w", &sipgo.ErrDialogResponse{Res: res})); got != 405 {
		t.Errorf("by pointer, wrapped: %d", got)
	}
	if got := responseStatus(errors.New("timeout")); got != 0 {
		t.Errorf("plain error: %d", got)
	}
}

// transfer() must tell a refusal (fall back to dialling) apart from a
// genuine failure (report it to the referrer), even through wrapping.
func TestOffloadRefusedIsDistinguishable(t *testing.T) {
	var refused *offloadRefused
	if !errors.As(fmt.Errorf("transfer: %w", &offloadRefused{"REFER rejected with 405"}), &refused) {
		t.Fatal("wrapped offloadRefused not recognised")
	}
	if refused.why != "REFER rejected with 405" {
		t.Fatalf("reason lost: %q", refused.why)
	}
	if errors.As(errors.New("connection reset"), &refused) {
		t.Fatal("a plain error must not read as a refusal")
	}
}

// wait() must hold for an in-progress offload and then know whether the
// legs were already released. No offload → proceed at once.
func TestAwaitOffloadStates(t *testing.T) {
	c := &bridgedCall{swapped: make(chan struct{}, 1)}
	if c.awaitOffload() {
		t.Fatal("no offload in progress must not read as offloaded")
	}
	c.beginOffload()
	done := make(chan bool, 1)
	go func() { done <- c.awaitOffload() }()
	select {
	case <-done:
		t.Fatal("awaitOffload returned before the offload was decided")
	default:
	}
	c.endOffload(true)
	if !<-done {
		t.Fatal("a successful offload must report the legs as released")
	}
	c.beginOffload()
	c.endOffload(false)
	if c.awaitOffload() {
		t.Fatal("a refused offload must let wait() tear down as usual")
	}
}
