package b2bua

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// A bridged call with pumps but no SIP sessions: watchMedia reads only the
// pumps' last-heard stamps and the transfer guards, so it can be driven
// without sockets. The SIP teardown it triggers is exercised by the harness
// (make harness-media-gone).
func mediaCall(pumps ...*pump) *bridgedCall {
	return &bridgedCall{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		callID:  "test",
		pumps:   pumps,
		swapped: make(chan struct{}, 1),
		dead:    make(chan struct{}),
	}
}

func livePump() *pump {
	p := &pump{}
	p.touch()
	return p
}

// silentFor returns a pump last heard from `d` ago.
func silentFor(d time.Duration) *pump {
	p := &pump{}
	p.lastReadAt.Store(time.Now().Add(-d).UnixMilli())
	return p
}

func deadWithin(t *testing.T, c *bridgedCall, d time.Duration) bool {
	t.Helper()
	select {
	case <-c.dead:
		return true
	case <-time.After(d):
		return false
	}
}

// The 2026-09-22 zombie: the app crashed mid-call, nothing was ever heard
// from it again, and no BYE could arrive. Both directions silent past the
// timeout is the one case that must end the call.
func TestWatchMediaEndsACallNobodyIsOn(t *testing.T) {
	c := mediaCall(silentFor(time.Second), silentFor(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 200*time.Millisecond)

	if !deadWithin(t, c, 2*time.Second) {
		t.Fatal("a call with no media either way was not ended")
	}
}

// One party still being heard keeps the call up, however long the other has
// been quiet: a one-way path (mute, a half-broken NAT) is a bad call, not a
// dead one, and hanging it up would be worse than leaving it.
func TestWatchMediaKeepsACallWithOneLiveParty(t *testing.T) {
	live := livePump()
	c := mediaCall(live, silentFor(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 200*time.Millisecond)

	// Keep one direction alive across several sweeps.
	for i := 0; i < 8; i++ {
		time.Sleep(50 * time.Millisecond)
		live.touch()
	}
	select {
	case <-c.dead:
		t.Fatal("ended a call one party was still on")
	default:
	}
}

// A held or muted leg sends no audio but still exchanges RTCP, and the
// RTCP hook touches the same stamp. That is what lets the check be "have we
// heard from them" rather than "are they sending audio", so hold needs no
// special case here.
func TestWatchMediaCountsRTCPAsAlive(t *testing.T) {
	held := livePump()  // sends nothing but RTCP
	other := livePump() // the party listening to the hold music
	c := mediaCall(held, other)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 200*time.Millisecond)

	for i := 0; i < 8; i++ {
		time.Sleep(50 * time.Millisecond)
		held.touch() // stands in for OnReadRTCP
		other.touch()
	}
	select {
	case <-c.dead:
		t.Fatal("ended a held call whose parties were still reachable")
	default:
	}
}

// During a transfer the legs are being replaced and one going quiet is
// expected — the same reason wait() consults these two guards before
// treating a leg's end as the call's end.
func TestWatchMediaIsSuppressedDuringATransfer(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *bridgedCall)
	}{
		{"PBX is completing it", func(c *bridgedCall) { c.offload = make(chan struct{}) }},
		{"we are completing it", func(c *bridgedCall) { c.handing = make(chan struct{}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := mediaCall(silentFor(time.Hour), silentFor(time.Hour))
			tc.set(c)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go c.watchMedia(ctx, 100*time.Millisecond)

			if deadWithin(t, c, 500*time.Millisecond) {
				t.Fatal("ended a call in the middle of a transfer")
			}
		})
	}
}

// Between pump restarts there is nothing to judge; the sweep must wait
// rather than read an empty set as silence.
func TestWatchMediaIgnoresACallWithNoPumps(t *testing.T) {
	c := mediaCall()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 100*time.Millisecond)

	if deadWithin(t, c, 400*time.Millisecond) {
		t.Fatal("ended a call whose pumps were between restarts")
	}
}

// A fresh pump counts as alive: media takes a moment to arrive after the
// answer, and a zero stamp would read as silent since 1970.
func TestNewPumpStartsAlive(t *testing.T) {
	if got := livePump().idleFor(time.Now()); got > time.Second {
		t.Fatalf("a new pump looks idle for %v", got)
	}
}

// The watcher stops with the call rather than outliving it.
func TestWatchMediaStopsWithItsContext(t *testing.T) {
	c := mediaCall(silentFor(time.Hour), silentFor(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.watchMedia(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop when the call ended")
	}
}
