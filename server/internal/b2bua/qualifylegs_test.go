package b2bua

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// fakeLeg is a party we can ask. `answer` is the final response it gives;
// 0 means it says nothing at all, so the ask times out exactly as it does
// against a phone that has crashed, gone out of range, or been suspended.
type fakeLeg struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	answer  int
	asks    int
	methods []sip.RequestMethod
	hangups int
}

func newFakeLeg(answer int) *fakeLeg {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeLeg{ctx: ctx, cancel: cancel, answer: answer}
}

func (f *fakeLeg) setAnswer(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = code
}

func (f *fakeLeg) asked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asks
}

func (f *fakeLeg) Id() string                { return "fake" }
func (f *fakeLeg) Context() context.Context  { return f.ctx }
func (f *fakeLeg) Media() *diago.DialogMedia { return nil }
func (f *fakeLeg) DialogSIP() *sipgo.Dialog  { return nil }
func (f *fakeLeg) Close() error              { return nil }
func (f *fakeLeg) RemoteContact() *sip.ContactHeader {
	return &sip.ContactHeader{Address: sip.Uri{User: "201", Host: "10.0.0.5", Port: 5061}}
}

func (f *fakeLeg) Hangup(context.Context) error {
	f.mu.Lock()
	f.hangups++
	f.mu.Unlock()
	f.cancel()
	return nil
}

func (f *fakeLeg) Do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	f.mu.Lock()
	f.asks++
	f.methods = append(f.methods, req.Method)
	code := f.answer
	f.mu.Unlock()
	if code == 0 {
		<-ctx.Done() // gone: nothing comes back and the ask times out
		return nil, ctx.Err()
	}
	return sip.NewResponseFromRequest(req, code, "", nil), nil
}

func qualifyCall(a, b *fakeLeg) *bridgedCall {
	c := &bridgedCall{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		callID:  "test",
		a:       &callLeg{name: "caller", sess: a},
		swapped: make(chan struct{}, 1),
		gone:    make(chan struct{}),
	}
	if b != nil {
		c.b = &callLeg{name: "callee", sess: b}
	}
	return c
}

func goneWithin(t *testing.T, c *bridgedCall, d time.Duration) bool {
	t.Helper()
	select {
	case <-c.gone:
		return true
	case <-time.After(d):
		return false
	}
}

// The question being asked is "are you there", and it is asked in SIP.
func TestQualifyAsksEachPartyWithOptions(t *testing.T) {
	a, b := newFakeLeg(200), newFakeLeg(200)
	c := qualifyCall(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	go c.qualifyLegs(ctx, 400*time.Millisecond)
	time.Sleep(350 * time.Millisecond)
	cancel()

	if a.asked() == 0 || b.asked() == 0 {
		t.Fatalf("both parties must be asked (a=%d b=%d)", a.asked(), b.asked())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.methods {
		if m != sip.OPTIONS {
			t.Fatalf("asked with %s, want OPTIONS", m)
		}
	}
}

// A party that stops answering ends the call, whatever the other is doing.
// This is the 2026-09-23 device test: the app was force-quit and the desk
// phone talked on into it.
func TestQualifyEndsTheCallWhenAPartyStopsAnswering(t *testing.T) {
	a, b := newFakeLeg(200), newFakeLeg(0) // the app, gone
	c := qualifyCall(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.qualifyLegs(ctx, 400*time.Millisecond)

	if !goneWithin(t, c, 3*time.Second) {
		t.Fatal("a call a party had left was not ended")
	}
	if a.asked() == 0 {
		t.Fatal("the surviving party should still have been asked")
	}
}

// Both answering is a live call, however quiet the audio: nothing here
// looks at media, which is the point — a held, muted or recvonly party
// answers an OPTIONS exactly as a talking one does.
func TestQualifyKeepsACallBothPartiesAnswer(t *testing.T) {
	c := qualifyCall(newFakeLeg(200), newFakeLeg(200))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.qualifyLegs(ctx, 200*time.Millisecond)

	if goneWithin(t, c, time.Second) {
		t.Fatal("ended a call both parties were answering")
	}
}

// Any final response means something is there. Stacks handle OPTIONS at
// very different levels — baresip answers from the user agent without
// consulting the dialog — so reading a particular code as "this call is
// over" would end live calls on the stacks that answer that way.
func TestQualifyTreatsAnyFinalResponseAsAlive(t *testing.T) {
	for _, code := range []int{200, 403, 405, 481, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			c := qualifyCall(newFakeLeg(200), newFakeLeg(code))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go c.qualifyLegs(ctx, 200*time.Millisecond)

			if goneWithin(t, c, time.Second) {
				t.Fatalf("%d ended the call; only silence means gone", code)
			}
		})
	}
}

// One lost OPTIONS must not end a call: a party has to miss every ask for
// the whole timeout. That is why the tolerance is the timeout itself
// rather than a retry count.
func TestQualifyToleratesAMissedAsk(t *testing.T) {
	a, b := newFakeLeg(200), newFakeLeg(200)
	c := qualifyCall(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.qualifyLegs(ctx, 600*time.Millisecond)

	// Silent for one ask, then answering again.
	time.Sleep(100 * time.Millisecond)
	b.setAnswer(0)
	time.Sleep(250 * time.Millisecond)
	b.setAnswer(200)

	if goneWithin(t, c, time.Second) {
		t.Fatal("one missed ask ended the call")
	}
}

// A leg a transfer swaps in is a different party and starts with a clean
// slate; the one swapped out is forgotten. Keying the record by leg is
// what makes a transfer need no special case here.
func TestQualifySwappedLegStartsFresh(t *testing.T) {
	a, b := newFakeLeg(200), newFakeLeg(0) // b is already not answering
	c := qualifyCall(a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.qualifyLegs(ctx, 600*time.Millisecond)

	// Before b's silence can end the call, a transfer replaces it.
	time.Sleep(200 * time.Millisecond)
	fresh := newFakeLeg(200)
	c.mu.Lock()
	c.b = &callLeg{name: "target", sess: fresh}
	c.mu.Unlock()

	if goneWithin(t, c, time.Second) {
		t.Fatal("the replaced leg's silence was charged to the one that took its place")
	}
	if fresh.asked() == 0 {
		t.Fatal("the new party was never asked")
	}
}

// A dialog that has already ended is wait()'s business, not ours: asking
// again would only wait out the probe timeout on every sweep.
func TestQualifyDoesNotAskAnEndedDialog(t *testing.T) {
	a, b := newFakeLeg(200), newFakeLeg(200)
	c := qualifyCall(a, b)
	b.cancel()
	before := b.asked()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.qualifyLegs(ctx, 400*time.Millisecond)
	time.Sleep(250 * time.Millisecond)

	if b.asked() != before {
		t.Fatal("asked a dialog that had already ended")
	}
}

// Echo (a party relayed to itself) has one leg, and it is still asked.
func TestQualifyAsksTheOnlyLegOfAnEchoCall(t *testing.T) {
	a := newFakeLeg(0)
	c := qualifyCall(a, nil)
	if c.b != nil {
		t.Fatal("echo should have no second leg")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.qualifyLegs(ctx, 400*time.Millisecond)

	if !goneWithin(t, c, 3*time.Second) {
		t.Fatal("an echo call the party left was not ended")
	}
}

// The qualifier stops with the call rather than outliving it.
func TestQualifyStopsWithItsContext(t *testing.T) {
	c := qualifyCall(newFakeLeg(200), newFakeLeg(200))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.qualifyLegs(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("qualifier did not stop when the call ended")
	}
}
