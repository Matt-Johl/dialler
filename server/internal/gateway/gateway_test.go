package gateway

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"dialler/server/internal/wire"
)

// The gateway is driven over net.Pipe so the suite needs no sockets and runs
// in any sandbox. Serve() is a thin accept loop over HandleConn and is
// exercised by the harness.

// staticAuth accepts a fixed device/token pair, plus "dev2"/"tok2" for the
// tests that need a second device.
type staticAuth struct{ device, token string }

func (a staticAuth) Authenticate(_ context.Context, d, t string) (bool, error) {
	return (d == a.device && t == a.token) || (d == "dev2" && t == "tok2"), nil
}

type harness struct {
	g   *Gateway
	ctx context.Context
}

func start(t *testing.T, cfg Config) *harness {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &harness{g: New(cfg, staticAuth{"dev1", "tok1"}), ctx: ctx}
}

type frame struct {
	e   wire.Envelope
	err error
}

type client struct {
	t      *testing.T
	conn   net.Conn
	frames chan frame
}

// dial opens an in-memory connection to the gateway. A reader goroutine
// drains frames so server writes never block on the synchronous pipe.
func (h *harness) dial(t *testing.T) *client {
	t.Helper()
	cs, ss := net.Pipe()
	done := make(chan struct{})
	go func() { h.g.HandleConn(h.ctx, ss); close(done) }()
	c := &client{t: t, conn: cs, frames: make(chan frame, 64)}
	go func() {
		for {
			e, err := wire.ReadFrame(cs)
			c.frames <- frame{e, err}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cs.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("HandleConn did not return after client close")
		}
	})
	return c
}

func (c *client) send(typ wire.Type, body any) {
	c.t.Helper()
	e, err := wire.New(typ, "cli-"+string(typ), time.Now(), body)
	if err != nil {
		c.t.Fatal(err)
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if err := wire.WriteFrame(c.conn, e); err != nil {
		c.t.Fatalf("send %s: %v", typ, err)
	}
}

func (c *client) recv() (wire.Envelope, error) {
	select {
	case f := <-c.frames:
		return f.e, f.err
	case <-time.After(2 * time.Second):
		c.t.Fatal("timed out waiting for a frame")
		return wire.Envelope{}, nil
	}
}

func (c *client) expect(typ wire.Type) wire.Envelope {
	c.t.Helper()
	e, err := c.recv()
	if err != nil {
		c.t.Fatalf("expected %s, got error %v", typ, err)
	}
	if e.Type != typ {
		c.t.Fatalf("expected %s, got %s body=%s", typ, e.Type, e.Body)
	}
	return e
}

func (c *client) expectClosed() {
	c.t.Helper()
	if _, err := c.recv(); err != io.EOF {
		c.t.Fatalf("expected close, got %v", err)
	}
}

// expectErrorAfterPings reads past the server's heartbeats to the error.
func (c *client) expectErrorAfterPings(code wire.ErrorCode) {
	c.t.Helper()
	for {
		e, err := c.recv()
		if err != nil {
			c.t.Fatalf("expected error %s, got %v", code, err)
		}
		if e.Type == wire.TypePing {
			continue // ours, and this client is deliberately not answering
		}
		if e.Type != wire.TypeError {
			c.t.Fatalf("expected %s, got %s body=%s", wire.TypeError, e.Type, e.Body)
		}
		var body wire.Error
		if err := e.DecodeBody(&body); err != nil {
			c.t.Fatal(err)
		}
		if body.Code != code {
			c.t.Fatalf("expected error %s, got %s", code, body.Code)
		}
		if body.Fatal {
			c.expectClosed()
		}
		return
	}
}

func (c *client) expectError(code wire.ErrorCode) {
	c.t.Helper()
	e := c.expect(wire.TypeError)
	var body wire.Error
	if err := e.DecodeBody(&body); err != nil {
		c.t.Fatal(err)
	}
	if body.Code != code {
		c.t.Fatalf("expected error %s, got %s", code, body.Code)
	}
	if body.Fatal {
		c.expectClosed()
	}
}

func (c *client) expectSilence() {
	c.t.Helper()
	select {
	case f := <-c.frames:
		c.t.Fatalf("unexpected frame %s (err=%v)", f.e.Type, f.err)
	case <-time.After(50 * time.Millisecond):
	}
}

func (c *client) hello(kind wire.ClientKind) wire.Welcome {
	c.t.Helper()
	return c.helloAs("dev1", "tok1", kind)
}

func (c *client) helloAs(deviceID, token string, kind wire.ClientKind) wire.Welcome {
	c.t.Helper()
	c.send(wire.TypeHello, wire.Hello{DeviceID: deviceID, Token: token, Client: kind})
	e := c.expect(wire.TypeWelcome)
	var w wire.Welcome
	if err := e.DecodeBody(&w); err != nil {
		c.t.Fatal(err)
	}
	return w
}

func TestHandshakeAndPing(t *testing.T) {
	h := start(t, Config{
		DirectoryVersion: func(d string) int64 { return 7 },
		SIPAccountFor: func(d string) *wire.SIPAccount {
			return &wire.SIPAccount{User: "201", Domain: "dialler", Host: "10.0.0.1", Port: 5061, Transport: "tls"}
		},
	})
	c := h.dial(t)
	w := c.hello(wire.ClientApp)
	if w.HeartbeatSeconds != 10 || w.DirectoryVersion != 7 || w.SessionID == "" {
		t.Fatalf("unexpected welcome %+v", w)
	}
	if w.SIP == nil || w.SIP.User != "201" || w.SIP.Port != 5061 {
		t.Fatalf("welcome should carry the device's SIP account: %+v", w.SIP)
	}
	if !h.g.Online("dev1") {
		t.Fatal("device should be online")
	}
	c.send(wire.TypePing, nil)
	c.expect(wire.TypePong)
}

func TestHandshakeRejections(t *testing.T) {
	h := start(t, Config{})

	c := h.dial(t)
	c.send(wire.TypeHello, wire.Hello{DeviceID: "dev1", Token: "wrong", Client: wire.ClientApp})
	c.expectError(wire.CodeUnauthorized)

	c = h.dial(t)
	c.send(wire.TypePing, nil)
	c.expectError(wire.CodeHelloExpected)

	c = h.dial(t)
	e, _ := wire.New(wire.TypeHello, "x", time.Now(), wire.Hello{DeviceID: "dev1", Token: "tok1", Client: wire.ClientApp})
	e.V = 2
	_ = wire.WriteFrame(c.conn, e)
	c.expectError(wire.CodeUnsupportedVersion)

	c = h.dial(t)
	c.send(wire.TypeHello, wire.Hello{DeviceID: "dev1", Token: "tok1", Client: "toaster"})
	c.expectError(wire.CodeBadFrame)
}

func TestHandshakeTimeout(t *testing.T) {
	h := start(t, Config{HandshakeTimeout: 50 * time.Millisecond})
	c := h.dial(t)
	c.expectClosed() // silent close, no error frame
}

// A client that never answers is still closed: the deadline now measures
// "does the far end answer when spoken to", so pings go out first and the
// error follows once three of them have gone unanswered.
func TestIdleTimeout(t *testing.T) {
	h := start(t, Config{Heartbeat: 40 * time.Millisecond})
	c := h.dial(t)
	c.hello(wire.ClientApp)
	c.expectErrorAfterPings(wire.CodeIdleTimeout)
}

// The heartbeat is the server's to send (2026-09-16). A client that only
// ever ANSWERS — never starting a frame of its own — must stay connected
// indefinitely: that is the Local Push extension, which iOS may not
// schedule to run a timer but does wake when data arrives.
func TestAnsweringClientIsNeverTimedOut(t *testing.T) {
	hb := 40 * time.Millisecond
	h := start(t, Config{Heartbeat: hb})
	c := h.dial(t)
	c.hello(wire.ClientApp)
	// Well past the 3×heartbeat deadline, answering nothing but pings.
	deadline := time.Now().Add(10 * hb)
	pings := 0
	for time.Now().Before(deadline) {
		e, err := c.recv()
		if err != nil {
			t.Fatalf("closed after %d pings, with %v", pings, err)
		}
		if e.Type != wire.TypePing {
			t.Fatalf("expected ping, got %s body=%s", e.Type, e.Body)
		}
		pings++
		c.send(wire.TypePong, nil)
	}
	if pings < 3 {
		t.Fatalf("only %d pings in 10 heartbeats; the server is not driving them", pings)
	}
}

func wakeAt(exp time.Time) wire.Wake {
	return wire.Wake{
		CallID:    "call1",
		From:      wire.Party{DisplayName: "Reception", URI: "sip:100@pbx"},
		To:        wire.Party{URI: "sip:201@dialler"},
		SIP:       wire.SIPTarget{Host: "dialler", Port: 5061, Transport: "tls"},
		ExpiresAt: exp,
	}
}

func TestWakeFansOutToBothKindsAndAcks(t *testing.T) {
	h := start(t, Config{})
	acks := make(chan wire.WakeAck, 1)
	h.g.OnWakeAck(func(_ string, a wire.WakeAck) { acks <- a })

	app := h.dial(t)
	app.hello(wire.ClientApp)
	ext := h.dial(t)
	ext.hello(wire.ClientExtension)

	if n := h.g.Wake("dev1", wakeAt(time.Now().Add(30*time.Second))); n != 2 {
		t.Fatalf("expected delivery to 2 connections, got %d", n)
	}
	for _, c := range []*client{app, ext} {
		e := c.expect(wire.TypeWake)
		var w wire.Wake
		_ = e.DecodeBody(&w)
		if w.CallID != "call1" || w.SIP.Transport != "tls" {
			t.Fatalf("bad wake %+v", w)
		}
	}
	ext.send(wire.TypeWakeAck, wire.WakeAck{CallID: "call1", Action: wire.WakeWillAnswer})
	select {
	case a := <-acks:
		if a.Action != wire.WakeWillAnswer {
			t.Fatalf("bad ack %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("ack callback not invoked")
	}

	ext.send(wire.TypeWakeAck, wire.WakeAck{CallID: "nope", Action: wire.WakeDecline})
	ext.expectError(wire.CodeUnknownCall) // non-fatal; connection stays up
	ext.send(wire.TypePing, nil)
	ext.expect(wire.TypePong)

	h.g.CancelWake("dev1", "call1", wire.CancelCallerHangup)
	app.expect(wire.TypeWakeCancel)
	ext.expect(wire.TypeWakeCancel)
}

func TestPendingWakeReplayedOnConnect(t *testing.T) {
	h := start(t, Config{})
	if n := h.g.Wake("dev1", wakeAt(time.Now().Add(30*time.Second))); n != 0 {
		t.Fatalf("offline device should report 0 deliveries, got %d", n)
	}
	expired := wakeAt(time.Now().Add(-time.Second))
	expired.CallID = "old"
	h.g.Wake("dev1", expired)

	c := h.dial(t)
	c.hello(wire.ClientExtension)
	e := c.expect(wire.TypeWake)
	var w wire.Wake
	_ = e.DecodeBody(&w)
	if w.CallID != "call1" {
		t.Fatalf("expected replay of call1, got %s", w.CallID)
	}
	c.expectSilence() // the expired wake was not replayed
}

// A wake survives its will_answer ack (a client reconnecting mid-ring is
// rung again), but once the B2BUA forgets it — the call was answered and is
// over — a later connect must not replay it: it rang the app for a dead
// call, and the app's answer then armed it to take the next INVITE unrung.
func TestForgottenWakeIsNotReplayed(t *testing.T) {
	h := start(t, Config{})
	app := h.dial(t)
	app.hello(wire.ClientApp)
	h.g.Wake("dev1", wakeAt(time.Now().Add(30*time.Second)))
	app.expect(wire.TypeWake)
	app.send(wire.TypeWakeAck, wire.WakeAck{CallID: "call1", Action: wire.WakeWillAnswer})
	app.send(wire.TypePing, nil)
	app.expect(wire.TypePong) // the ack was processed

	h.g.ForgetWake("dev1", "call1")
	app.expectSilence() // forgetting is silent: the live call must not be ended

	again := h.dial(t)
	again.hello(wire.ClientApp)
	again.expectSilence() // nothing to replay
}

// The locked-phone race (seen 2026-09-13 01:31): the wake goes to the
// extension, which launches the app; the caller hangs up before the app's
// own session is up; the app then connects and must be told the call is
// gone, or it rings on.
func TestCancelIssuedBeforeTheAppConnectedIsReplayed(t *testing.T) {
	h := start(t, Config{})
	ext := h.dial(t)
	ext.hello(wire.ClientExtension)
	h.g.Wake("dev1", wakeAt(time.Now().Add(30*time.Second)))
	ext.expect(wire.TypeWake)

	h.g.CancelWake("dev1", "call1", wire.CancelCallerHangup)
	ext.expect(wire.TypeWakeCancel) // the live connection hears it at once

	app := h.dial(t)
	app.hello(wire.ClientApp)
	app.expect(wire.TypeWakeCancel) // no wake to replay, but its cancel
	app.expectSilence()

	// A fresh wake for the same call id must not be followed by the stale cancel.
	h.g.Wake("dev1", wakeAt(time.Now().Add(30*time.Second)))
	app.expect(wire.TypeWake)
	app.expectSilence()
}

func TestReplayedCancelExpiresWithItsWake(t *testing.T) {
	var mu sync.Mutex
	base := time.Now()
	skew := time.Duration(0)
	h := start(t, Config{Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return base.Add(skew) }})
	h.g.Wake("dev1", wakeAt(base.Add(30*time.Second))) // nobody connected
	h.g.CancelWake("dev1", "call1", wire.CancelTimeout)

	mu.Lock()
	skew = 30*time.Second + cancelGrace + time.Second // past the wake's expiry and the grace
	mu.Unlock()
	late := h.dial(t)
	late.hello(wire.ClientApp)
	late.expectSilence() // a ring that old has ended on its own; nothing to say
}

func TestSupersedeSameKind(t *testing.T) {
	h := start(t, Config{})
	var mu sync.Mutex
	var events []PresenceEvent
	h.g.OnPresence(func(ev PresenceEvent) { mu.Lock(); events = append(events, ev); mu.Unlock() })

	first := h.dial(t)
	first.hello(wire.ClientExtension)
	second := h.dial(t)
	second.hello(wire.ClientExtension)
	first.expectError(wire.CodeSuperseded)

	// The device stays online through the handover, and the newer session works.
	if !h.g.Online("dev1") {
		t.Fatal("device went offline during supersede")
	}
	second.send(wire.TypePing, nil)
	second.expect(wire.TypePong)

	// Presence: two "online" events and no "offline" from the old session.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(events)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("expected 2 presence events, got %+v", events)
	}
	for _, ev := range events {
		if !ev.Online {
			t.Fatalf("superseded session emitted offline: %+v", events)
		}
	}
}

func TestDetachEmitsOffline(t *testing.T) {
	h := start(t, Config{})
	events := make(chan PresenceEvent, 4)
	h.g.OnPresence(func(ev PresenceEvent) { events <- ev })
	c := h.dial(t)
	c.hello(wire.ClientApp)
	<-events // online
	_ = c.conn.Close()
	select {
	case ev := <-events:
		if ev.Online || ev.Kind != wire.ClientApp {
			t.Fatalf("bad offline event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no offline event")
	}
	if h.g.Online("dev1") {
		t.Fatal("device still online after close")
	}
}

// A directory change reaches every session of the device it belongs to
// and no session of any other device (SPEC §6 item 7, §4.8 isolation).
func TestDirectoryChangeReachesOnlyItsDevice(t *testing.T) {
	h := start(t, Config{})
	a := h.dial(t)
	a.hello(wire.ClientApp)
	b := h.dial(t)
	b.hello(wire.ClientExtension)
	other := h.dial(t)
	other.helloAs("dev2", "tok2", wire.ClientApp)
	h.g.NotifyDirectory("dev1", 99)
	for _, c := range []*client{a, b} {
		e := c.expect(wire.TypeDirectoryChanged)
		var d wire.DirectoryChanged
		_ = e.DecodeBody(&d)
		if d.Version != 99 {
			t.Fatalf("bad version %d", d.Version)
		}
	}
	// The other device hears nothing.
	other.expectSilence()
}

// A rotated credential drops that device's sessions with unauthorized and
// leaves another device's alone (SPEC §4.8 isolation).
func TestDisconnectClosesOnlyThatDevice(t *testing.T) {
	h := start(t, Config{})
	a := h.dial(t)
	a.hello(wire.ClientApp)
	other := h.dial(t)
	other.helloAs("dev2", "tok2", wire.ClientApp)
	h.g.Disconnect("dev1")
	a.expectError(wire.CodeUnauthorized)
	other.expectSilence()
	if h.g.Online("dev1") || !h.g.Online("dev2") {
		t.Fatal("dev1 should be offline and dev2 online")
	}
}

func TestOversizedFrameIsFatal(t *testing.T) {
	h := start(t, Config{})
	c := h.dial(t)
	c.hello(wire.ClientApp)
	_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = c.conn.Write([]byte{0xff, 0xff, 0xff, 0xff})
	c.expectError(wire.CodeBadFrame)
}
