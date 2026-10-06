package gateway

import (
	"testing"

	"dialpark/server/internal/wire"
)

// errorMessage reads the next frame as an error and returns its body.
func (c *client) errorBody() wire.Error {
	c.t.Helper()
	e := c.expect(wire.TypeError)
	var body wire.Error
	if err := e.DecodeBody(&body); err != nil {
		c.t.Fatal(err)
	}
	return body
}

// A device whose credential is good but which holds no licence seat is
// refused after the credential check and before any welcome, with the
// reason as the message, on the one error code every app can decode.
func TestLicenceGateRefusesBeforeTheWelcome(t *testing.T) {
	h := start(t, Config{Licensed: func(id string) (bool, string) {
		if id == "dev2" {
			return true, ""
		}
		return false, "no licence seat"
	}})
	c := h.dial(t)
	c.send(wire.TypeHello, wire.Hello{DeviceID: "dev1", Token: "tok1", Client: wire.ClientApp})
	body := c.errorBody()
	if body.Code != wire.CodeUnauthorized || body.Message != "no licence seat" || !body.Fatal {
		t.Fatalf("refusal = %+v, want fatal unauthorized 'no licence seat'", body)
	}
	c.expectClosed()
	if n := len(h.g.Sessions()["dev1"]); n != 0 {
		t.Fatalf("a refused device was attached: %d sessions", n)
	}
	// A wake for it goes nowhere, as for any device with no session.
	if delivered := h.g.Wake("dev1", wire.Wake{CallID: "c1"}); delivered != 0 {
		t.Fatalf("wake delivered to %d sessions of a refused device", delivered)
	}
	// The licensed device is untouched.
	c2 := h.dial(t)
	c2.helloAs("dev2", "tok2", wire.ClientApp)

	// A bad credential is still the credential's refusal, not the licence's:
	// the gate runs after Authenticate.
	c3 := h.dial(t)
	c3.send(wire.TypeHello, wire.Hello{DeviceID: "dev1", Token: "wrong", Client: wire.ClientApp})
	if body := c3.errorBody(); body.Message != "unknown device or bad token" {
		t.Fatalf("bad token with no seat: %+v", body)
	}
}

func TestWelcomeCarriesTheLicence(t *testing.T) {
	h := start(t, Config{LicenceFor: func() string { return "DP1.eyJ2IjoxfQ.c2ln" }})
	w := h.dial(t).hello(wire.ClientApp)
	if w.Licence != "DP1.eyJ2IjoxfQ.c2ln" {
		t.Fatalf("welcome.licence = %q", w.Licence)
	}
	// Without a source there is no field at all, as for every other
	// optional part of the welcome.
	h2 := start(t, Config{})
	if w := h2.dial(t).hello(wire.ClientApp); w.Licence != "" {
		t.Fatalf("welcome.licence = %q with no source", w.Licence)
	}
}

// Disconnect names its reason, so a device losing its seat reads
// "licence expired" rather than "credential rotated".
func TestDisconnectCarriesTheReason(t *testing.T) {
	h := start(t, Config{})
	c := h.dial(t)
	c.hello(wire.ClientApp)
	h.g.DisconnectWith("dev1", "licence expired")
	body := c.errorBody()
	if body.Code != wire.CodeUnauthorized || body.Message != "licence expired" || !body.Fatal {
		t.Fatalf("disconnect = %+v", body)
	}
	c.expectClosed()
}
