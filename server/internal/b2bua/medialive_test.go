package b2bua

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// A bridged call with legs and pumps but no SIP sessions: watchMedia reads
// only the pumps' last-heard stamps, the legs they read from, and the
// transfer/hold guards, so it can be driven without sockets. The SIP
// teardown it triggers is exercised by the harness (make harness-media-gone).
//
// Pump order matches startLocked: pumps[0] reads from a, pumps[1] from b.
func mediaCall(pumps ...*pump) *bridgedCall {
	c := &bridgedCall{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		callID:  "test",
		a:       &callLeg{name: "caller"},
		b:       &callLeg{name: "callee"},
		pumps:   pumps,
		swapped: make(chan struct{}, 1),
		dead:    make(chan struct{}),
	}
	if len(pumps) < 2 {
		c.b = nil // echo: one pump, reading from a
	}
	return c
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

// The 2026-09-23 device test, and the case the first cut of this missed.
// The app was force-quit mid-call; the desk phone carried on sending 100
// packets every 2 s into a leg whose every write failed. Judging the call
// as a whole ("has everyone gone quiet?") kept it up for as long as anyone
// watched. One party gone is enough: a two-party call needs both.
func TestWatchMediaEndsACallOnePartyLeft(t *testing.T) {
	talking := livePump()                           // the desk phone, still sending
	c := mediaCall(talking, silentFor(time.Second)) // the app, gone
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 200*time.Millisecond)

	// Keep the survivor talking across several sweeps; it must not save the call.
	go func() {
		for i := 0; i < 20; i++ {
			talking.touch()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if !deadWithin(t, c, 2*time.Second) {
		t.Fatal("a call the callee had left was not ended")
	}
}

// The 2026-09-22 zombie: both parties gone, 14½ hours of relay.
func TestWatchMediaEndsACallNobodyIsOn(t *testing.T) {
	c := mediaCall(silentFor(time.Second), silentFor(time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 200*time.Millisecond)

	if !deadWithin(t, c, 2*time.Second) {
		t.Fatal("a call with no media either way was not ended")
	}
}

// Both parties still being heard is a live call, however quiet the audio.
// A party that is merely silent — muted, comfort noise, nothing to say —
// still sends RTP or RTCP, and both touch the same stamp.
func TestWatchMediaKeepsACallBothPartiesAreOn(t *testing.T) {
	a, b := livePump(), livePump()
	c := mediaCall(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 200*time.Millisecond)

	for i := 0; i < 8; i++ {
		time.Sleep(50 * time.Millisecond)
		a.touch()
		b.touch() // stands in for RTP or the OnReadRTCP hook
	}
	select {
	case <-c.dead:
		t.Fatal("ended a call both parties were still on")
	default:
	}
}

// The party who pressed hold has stopped sending on purpose and is being
// played our music; their silence says nothing about whether they are
// there. Without this a held call would be dropped at the timeout — the
// harness hold tests hold for ~20 s and would never have caught it.
func TestWatchMediaDoesNotJudgeTheHeldParty(t *testing.T) {
	held, other := silentFor(time.Hour), livePump()
	c := mediaCall(held, other)
	c.heldBy = c.a // the caller pressed hold; pumps[0] reads from them
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 100*time.Millisecond)

	// Keep the listening party alive for the whole assertion window, not a
	// fixed number of ticks: stopping early would make this pass for the
	// wrong reason (or fail for one).
	stop := make(chan struct{})
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				other.touch()
			}
		}
	}()
	if deadWithin(t, c, 600*time.Millisecond) {
		close(stop)
		t.Fatal("dropped a call that was simply on hold")
	}
	close(stop)

	// The other party leaving while the first holds still ends it: hold
	// excuses the holder's silence, nobody else's.
	c.mu.Lock()
	c.pumps[1] = silentFor(time.Hour)
	c.mu.Unlock()
	if !deadWithin(t, c, 2*time.Second) {
		t.Fatal("a held call whose other party left was not ended")
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

// Echo (the server relaying a party to itself) has one pump, reading from
// the only leg there is.
func TestWatchMediaEndsAnEchoCallThePartyLeft(t *testing.T) {
	c := mediaCall(silentFor(time.Second))
	if c.b != nil {
		t.Fatal("echo should have no second leg")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.watchMedia(ctx, 200*time.Millisecond)

	if !deadWithin(t, c, 2*time.Second) {
		t.Fatal("an echo call the party left was not ended")
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
