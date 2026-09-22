package pbxline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRegistrar stands in for the SIP stack: it records what it was asked to
// do and answers however the test wants.
type fakeRegistrar struct {
	mu       sync.Mutex
	calls    []Line
	expiries []time.Duration
	unreg    []Line
	// answer is consulted per call, n being 1 for the first.
	answer func(n int, l Line) (Registration, error)
	each   chan struct{}
}

func newFakeRegistrar(answer func(n int, l Line) (Registration, error)) *fakeRegistrar {
	return &fakeRegistrar{answer: answer, each: make(chan struct{}, 256)}
}

func (f *fakeRegistrar) Register(_ context.Context, l Line, expiry time.Duration) (Registration, error) {
	f.mu.Lock()
	f.calls = append(f.calls, l)
	f.expiries = append(f.expiries, expiry)
	n := len(f.calls)
	answer := f.answer
	f.mu.Unlock()
	select {
	case f.each <- struct{}{}:
	default:
	}
	if answer == nil {
		return Registration{Expiry: expiry}, nil
	}
	return answer(n, l)
}

func (f *fakeRegistrar) Unregister(_ context.Context, l Line) error {
	f.mu.Lock()
	f.unreg = append(f.unreg, l)
	f.mu.Unlock()
	select {
	case f.each <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakeRegistrar) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRegistrar) unregistered() []Line {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Line(nil), f.unreg...)
}

// testManager is a manager with every duration wound down and the jitter
// pinned, so the tests assert behaviour rather than wait on a clock.
func testManager(t *testing.T, r Registrar) *Manager {
	t.Helper()
	return New(Config{
		Registrar:   r,
		Expiry:      40 * time.Millisecond,
		MinRefresh:  time.Millisecond,
		BackoffBase: time.Millisecond,
		BackoffCap:  5 * time.Millisecond,
		StartSpread: 0,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Rand:        func() float64 { return 0 },
	})
}

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func stateOf(t *testing.T, m *Manager, user string) State {
	t.Helper()
	s, ok := m.Status(user)
	if !ok {
		t.Fatalf("no status for %s", user)
	}
	return s.State
}

func TestALineRegistersAndRefreshesBeforeItExpires(t *testing.T) {
	f := newFakeRegistrar(nil)
	m := testManager(t, f)
	m.Put(Line{User: "201", DigestUser: "matt", Secret: "s"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, "the line to register", func() bool { return stateOf(t, m, "201") == StateRegistered })
	// The PBX granted 40 ms, so a refresh is due after 30 ms: the line must
	// re-register on its own, without anything prompting it.
	waitFor(t, "a refresh", func() bool { return f.count() >= 3 })

	s, _ := m.Status("201")
	if s.ExpiresAt.IsZero() {
		t.Error("a registered line should report when its binding lapses")
	}
	if s.DN != "201" {
		t.Errorf("DN should default to the user, got %q", s.DN)
	}
}

func TestARefusedLineStopsUntilTheCredentialChanges(t *testing.T) {
	f := newFakeRegistrar(func(_ int, l Line) (Registration, error) {
		if l.Secret == "right" {
			return Registration{Expiry: time.Hour}, nil
		}
		return Registration{}, &Refused{Status: 401, Reason: "credential refused"}
	})
	m := testManager(t, f)
	m.Put(Line{User: "201", DigestUser: "matt", Secret: "wrong"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, "the line to be refused", func() bool { return stateOf(t, m, "201") == StateRefused })

	// The point of the latch: a wrong password must not be retried. Give it
	// far longer than any backoff would have taken.
	time.Sleep(50 * time.Millisecond)
	if n := f.count(); n != 1 {
		t.Fatalf("a refused line retried %d times; it must stop until the credential changes", n)
	}

	// An administrator fixes it: the line must pick it up at once, without
	// waiting for a refresh interval that is not running.
	m.Put(Line{User: "201", DigestUser: "matt", Secret: "right"})
	waitFor(t, "the corrected line to register", func() bool { return stateOf(t, m, "201") == StateRegistered })
}

func TestATransientFailureRetriesAndRecovers(t *testing.T) {
	f := newFakeRegistrar(func(n int, _ Line) (Registration, error) {
		if n < 3 {
			return Registration{}, errors.New("connection refused")
		}
		return Registration{Expiry: time.Hour}, nil
	})
	m := testManager(t, f)
	m.Put(Line{User: "201", DigestUser: "matt", Secret: "s"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, "the line to recover", func() bool { return stateOf(t, m, "201") == StateRegistered })
	if n := f.count(); n < 3 {
		t.Fatalf("expected the line to retry through its failures, saw %d attempts", n)
	}
	s, _ := m.Status("201")
	if s.Error != "" || s.Attempts != 0 {
		t.Errorf("a recovered line should clear its error, got %+v", s)
	}
}

func TestAnIdenticalWriteDoesNotDisturbALiveLine(t *testing.T) {
	f := newFakeRegistrar(func(_ int, _ Line) (Registration, error) {
		return Registration{Expiry: time.Hour}, nil // no refresh will be due
	})
	m := testManager(t, f)
	l := Line{User: "201", DN: "201", DigestUser: "matt", Secret: "s"}
	m.Put(l)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, "the line to register", func() bool { return f.count() == 1 })

	// Re-applying the device store's view (a restart of nothing, an admin
	// write to another field) must not drop or re-send a registration.
	m.Set([]Line{l})
	time.Sleep(20 * time.Millisecond)
	if n := f.count(); n != 1 {
		t.Fatalf("an identical write re-registered the line (%d sends)", n)
	}

	// A changed secret must.
	m.Put(Line{User: "201", DN: "201", DigestUser: "matt", Secret: "s2"})
	waitFor(t, "the changed line to re-register", func() bool { return f.count() == 2 })
}

func TestSetRemovesLinesThatAreGoneAndDropsTheirBinding(t *testing.T) {
	f := newFakeRegistrar(func(_ int, _ Line) (Registration, error) {
		return Registration{Expiry: time.Hour}, nil
	})
	m := testManager(t, f)
	m.Set([]Line{{User: "201", DigestUser: "a", Secret: "s"}, {User: "202", DigestUser: "b", Secret: "s"}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, "both lines to register", func() bool { return f.count() == 2 })

	m.Set([]Line{{User: "201", DigestUser: "a", Secret: "s"}})
	waitFor(t, "the removed line to be unregistered", func() bool { return len(f.unregistered()) == 1 })
	if got := f.unregistered()[0].User; got != "202" {
		t.Errorf("unregistered the wrong line: %q", got)
	}
	if _, ok := m.Status("202"); ok {
		t.Error("a removed line should not still be reported")
	}
	if stateOf(t, m, "201") != StateRegistered {
		t.Error("removing one line disturbed another")
	}
}

func TestCredentialsAndNumber(t *testing.T) {
	m := testManager(t, newFakeRegistrar(nil))
	m.Put(Line{User: "201", DigestUser: "matt", Secret: "s"})
	m.Put(Line{User: "202", DN: "7002", DigestUser: "jo", Secret: "s2"})

	if u, p, ok := m.Credentials("201"); !ok || u != "matt" || p != "s" {
		t.Errorf("credentials for 201 = %q %q %v", u, p, ok)
	}
	if dn, ok := m.Number("201"); !ok || dn != "201" {
		t.Errorf("number for 201 = %q %v; DN should default to the user", dn, ok)
	}
	if dn, ok := m.Number("202"); !ok || dn != "7002" {
		t.Errorf("number for 202 = %q %v; an explicit DN should win", dn, ok)
	}
	if _, _, ok := m.Credentials("999"); ok {
		t.Error("an unknown user must not yield credentials")
	}

	// Trunk mode has no manager at all, and the call path should not have
	// to know that.
	var nilM *Manager
	if _, _, ok := nilM.Credentials("201"); ok {
		t.Error("a nil manager must report no credentials")
	}
	if _, ok := nilM.Number("201"); ok {
		t.Error("a nil manager must report no number")
	}
	if nilM.Statuses() != nil {
		t.Error("a nil manager must report no statuses")
	}
}

func TestStatusNeverCarriesTheSecret(t *testing.T) {
	m := testManager(t, newFakeRegistrar(nil))
	m.Put(Line{User: "201", DigestUser: "matt", Secret: "hunter2"})
	b, err := json.Marshal(m.Statuses())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hunter2") {
		t.Fatalf("the secret reached a status body: %s", b)
	}
}

func TestStatusesAreOrderedByUser(t *testing.T) {
	m := testManager(t, newFakeRegistrar(nil))
	for _, u := range []string{"203", "201", "202"} {
		m.Put(Line{User: u, DigestUser: u, Secret: "s"})
	}
	got := m.Statuses()
	if len(got) != 3 || got[0].User != "201" || got[1].User != "202" || got[2].User != "203" {
		t.Fatalf("statuses out of order: %+v", got)
	}
}

func TestStopDropsEveryBinding(t *testing.T) {
	f := newFakeRegistrar(func(_ int, _ Line) (Registration, error) {
		return Registration{Expiry: time.Hour}, nil
	})
	m := testManager(t, f)
	m.Set([]Line{{User: "201", DigestUser: "a", Secret: "s"}})

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	waitFor(t, "the line to register", func() bool { return f.count() == 1 })

	cancel()
	m.Stop()
	// Stop must return once the loops are done, not leave one running.
	before := f.count()
	time.Sleep(20 * time.Millisecond)
	if f.count() != before {
		t.Fatal("a line kept registering after Stop")
	}
	// And the binding must be dropped. Without it the exchange goes on
	// sending calls to a contact nothing is listening on until the
	// registration expires — up to an hour of callers ringing out.
	un := f.unregistered()
	if len(un) != 1 || un[0].User != "201" {
		t.Fatalf("Stop left the line registered on the PBX: %+v", un)
	}
}

// A manager that never started holds no bindings, so stopping it must not
// send an unregister for one.
func TestStopBeforeStartUnregistersNothing(t *testing.T) {
	f := newFakeRegistrar(nil)
	m := testManager(t, f)
	m.Put(Line{User: "201", DigestUser: "a", Secret: "s"})
	m.Stop()
	if n := len(f.unregistered()); n != 0 {
		t.Fatalf("unregistered %d lines that were never registered", n)
	}
}

// A refusal a Registrar wrapped in context is still a refusal: unwrapped by
// errors.As, not by type assertion, or it would be retried for ever against
// an exchange that has already said no.
func TestAWrappedRefusalStillLatches(t *testing.T) {
	f := newFakeRegistrar(func(_ int, _ Line) (Registration, error) {
		return Registration{}, fmt.Errorf("register sip:pbx: %w", &Refused{Status: 403, Reason: "forbidden"})
	})
	m := testManager(t, f)
	m.Put(Line{User: "201", DigestUser: "a", Secret: "s"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	waitFor(t, "the wrapped refusal to latch", func() bool { return stateOf(t, m, "201") == StateRefused })
	time.Sleep(30 * time.Millisecond)
	if n := f.count(); n != 1 {
		t.Fatalf("a wrapped refusal was retried %d times", n)
	}
}

func TestRefreshAfter(t *testing.T) {
	for _, tc := range []struct {
		granted, floor, want time.Duration
	}{
		{time.Hour, 5 * time.Second, 45 * time.Minute},
		{120 * time.Second, 5 * time.Second, 90 * time.Second},
		// A PBX granting almost nothing must not turn into a REGISTER
		// storm: the floor wins.
		{time.Second, 5 * time.Second, 5 * time.Second},
	} {
		if got := refreshAfter(tc.granted, tc.floor); got != tc.want {
			t.Errorf("refreshAfter(%s, %s) = %s, want %s", tc.granted, tc.floor, got, tc.want)
		}
	}
}

func TestBackoff(t *testing.T) {
	base, limit := 5*time.Second, 5*time.Minute
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{0, 5 * time.Second}, // defensive: treated as the first
		{1, 5 * time.Second},
		{2, 10 * time.Second},
		{3, 20 * time.Second},
		{6, 160 * time.Second},
		{7, limit},
		{50, limit},
	} {
		if got := backoff(tc.attempt, base, limit); got != tc.want {
			t.Errorf("backoff(%d) = %s, want %s", tc.attempt, got, tc.want)
		}
	}
}
