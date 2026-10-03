package licence

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logBuffer) String() string     { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func (l *logBuffer) count(s string) int { return strings.Count(l.String(), s) }

func openManager(t *testing.T, dir string, clock *fakeClock, logs *logBuffer) *Manager {
	t.Helper()
	return Open(filepath.Join(dir, "licence"), sample().InstallID, clock.now, slog.New(slog.NewTextHandler(logs, nil)))
}

func TestOpenWithNoFileIsMissingAndZeroSeats(t *testing.T) {
	clock := &fakeClock{t: t0}
	m := openManager(t, t.TempDir(), clock, &logBuffer{})
	if m.State() != StateMissing || m.Seats() != 0 || m.Token() != "" {
		t.Fatalf("missing licence: state=%s seats=%d token=%q", m.State(), m.Seats(), m.Token())
	}
	s := m.Summary(0)
	if s.State != StateMissing || s.InstallID != sample().InstallID {
		t.Fatalf("summary %+v", s)
	}
}

func TestOpenWithGarbageIsInvalidNotFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "licence"), []byte("not a licence"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	m := openManager(t, dir, &fakeClock{t: t0}, logs)
	if m.State() != StateInvalid || m.Seats() != 0 {
		t.Fatalf("garbage file: state=%s seats=%d", m.State(), m.Seats())
	}
	if m.Summary(0).Error == "" {
		t.Error("the summary must say why the stored licence is unusable")
	}
}

func TestInstallWritesTheFileAndFiresOnce(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: t0}
	m := openManager(t, dir, clock, &logBuffer{})
	var fired []int
	m.OnChange(func(seats int) { fired = append(fired, seats) })

	tok := mustSign(t, sample())
	s, err := m.Install(tok)
	if err != nil {
		t.Fatal(err)
	}
	if s.State != StateActive || s.Seats != 10 || s.Customer != "Example Ltd" || s.ID != "lic_TESTX001" {
		t.Fatalf("summary after install: %+v", s)
	}
	st, err := os.Stat(filepath.Join(dir, "licence"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("licence file: %v mode %v", err, st.Mode())
	}
	if m.Token() != tok {
		t.Error("Token must return exactly what was installed")
	}
	if len(fired) != 1 || fired[0] != 10 {
		t.Fatalf("OnChange calls = %v, want [10]", fired)
	}
	// The same licence again changes nothing and fires nothing.
	if _, err := m.Install(tok); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 {
		t.Fatalf("re-installing the same licence fired OnChange: %v", fired)
	}
	// A different seat count fires with the new number.
	l := sample()
	l.Seats = 3
	l.ID = "lic_TESTX002"
	if _, err := m.Install(mustSign(t, l)); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 2 || fired[1] != 3 {
		t.Fatalf("OnChange calls = %v, want [10 3]", fired)
	}

	// And it comes back on the next Open.
	m2 := openManager(t, dir, clock, &logBuffer{})
	if m2.State() != StateActive || m2.Seats() != 3 {
		t.Fatalf("reopened: state=%s seats=%d", m2.State(), m2.Seats())
	}
}

func TestInstallRefusals(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: t0}
	m := openManager(t, dir, clock, &logBuffer{})
	good := mustSign(t, sample())
	if _, err := m.Install(good); err != nil {
		t.Fatal(err)
	}
	wrong := sample()
	wrong.InstallID = "ffffffffffffffffffffffffffffffff"
	expired := sample()
	expired.ValidUntil = t0.Add(-time.Hour)
	expired.IssuedAt = t0.Add(-2 * time.Hour)
	cases := map[string]struct {
		tok  string
		want error
	}{
		"garbage":       {"DL1.zzz.zzz", ErrFormat},
		"wrong install": {mustSign(t, wrong), ErrWrongInstall},
		"expired":       {mustSign(t, expired), ErrExpired},
	}
	for name, c := range cases {
		_, err := m.Install(c.tok)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	if !strings.Contains(func() string { _, e := m.Install(mustSign(t, wrong)); return e.Error() }(), "ffffffffffffffffffffffffffffffff") {
		t.Error("the wrong-install refusal must name the id the licence was issued for")
	}
	// A refused install leaves the previous licence in force, on disk too.
	if m.State() != StateActive || m.Seats() != 10 || m.Token() != good {
		t.Fatalf("a refusal disturbed the installed licence: %s %d", m.State(), m.Seats())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "licence"))
	if strings.TrimSpace(string(b)) != good {
		t.Error("a refusal rewrote the licence file")
	}
}

func TestInstallWriteFailureKeepsThePreviousLicence(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: t0}
	m := openManager(t, dir, clock, &logBuffer{})
	good := mustSign(t, sample())
	if _, err := m.Install(good); err != nil {
		t.Fatal(err)
	}
	// Make the directory unwritable so the temp file cannot be created.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	l := sample()
	l.Seats = 2
	l.ID = "lic_TESTX003"
	fired := 0
	m.OnChange(func(int) { fired++ })
	if _, err := m.Install(mustSign(t, l)); err == nil {
		t.Fatal("expected the write to fail")
	}
	if m.Seats() != 10 || m.Token() != good || fired != 0 {
		t.Fatalf("a failed write changed the licence in memory: seats=%d fired=%d", m.Seats(), fired)
	}
}

// Expiry is observed in place by the tick: one OnChange(0), one error line,
// and a warning once a day inside the last 30 days.
func TestTickWarnsThenExpiresOnce(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: t0}
	logs := &logBuffer{}
	m := openManager(t, dir, clock, logs)
	var fired []int
	m.OnChange(func(seats int) { fired = append(fired, seats) })
	l := sample()
	l.ValidUntil = t0.Add(40 * 24 * time.Hour)
	if _, err := m.Install(mustSign(t, l)); err != nil {
		t.Fatal(err)
	}

	at := func(when time.Time) { clock.set(when); m.tick(when) }
	at(t0.Add(24 * time.Hour)) // 39 days left: nothing to say
	if logs.count("expires soon") != 0 {
		t.Fatalf("warned outside the window:\n%s", logs)
	}
	at(t0.Add(11 * 24 * time.Hour)) // 29 days left
	at(t0.Add(11*24*time.Hour + time.Hour))
	at(t0.Add(11*24*time.Hour + 23*time.Hour))
	if n := logs.count("expires soon"); n != 1 {
		t.Fatalf("warnings inside one day = %d, want 1:\n%s", n, logs)
	}
	at(t0.Add(12*24*time.Hour + time.Hour)) // a day and an hour later
	if n := logs.count("expires soon"); n != 2 {
		t.Fatalf("warnings after a day = %d, want 2:\n%s", n, logs)
	}
	if m.State() != StateExpiring || m.Seats() != 10 {
		t.Fatalf("expiring licence must keep its seats: %s %d", m.State(), m.Seats())
	}

	at(l.ValidUntil)
	at(l.ValidUntil.Add(time.Hour))
	at(l.ValidUntil.Add(48 * time.Hour))
	if m.State() != StateExpired || m.Seats() != 0 {
		t.Fatalf("after expiry: %s %d", m.State(), m.Seats())
	}
	if len(fired) != 2 || fired[0] != 10 || fired[1] != 0 {
		t.Fatalf("OnChange calls = %v, want [10 0]", fired)
	}
	if n := logs.count("licence: expired"); n != 1 {
		t.Fatalf("expiry errors = %d, want exactly 1:\n%s", n, logs)
	}
	if m.Token() != "" {
		t.Error("an expired licence must not be handed to phones")
	}
	s := m.Summary(4)
	if s.State != StateExpired || s.SeatsUsed != 4 || s.DaysLeft != 0 || s.ID != "lic_TESTX001" {
		t.Fatalf("expired summary must still say what expired: %+v", s)
	}
	// Renewal resumes at once.
	l.ValidUntil = l.ValidUntil.Add(365 * 24 * time.Hour)
	l.ID = "lic_TESTX004"
	if _, err := m.Install(mustSign(t, l)); err != nil {
		t.Fatal(err)
	}
	if m.State() != StateActive || m.Seats() != 10 || len(fired) != 3 || fired[2] != 10 {
		t.Fatalf("renewal: %s %d fired=%v", m.State(), m.Seats(), fired)
	}
}

func TestInstallCloseToExpiryIsAcceptedAndThenExpires(t *testing.T) {
	clock := &fakeClock{t: t0}
	m := openManager(t, t.TempDir(), clock, &logBuffer{})
	l := sample()
	l.ValidUntil = t0.Add(15 * time.Minute)
	if _, err := m.Install(mustSign(t, l)); err != nil {
		t.Fatalf("15 minutes of validity is still validity: %v", err)
	}
	if m.State() != StateExpiring {
		t.Errorf("state %s, want expiring", m.State())
	}
	clock.set(t0.Add(16 * time.Minute))
	m.tick(clock.now())
	if m.State() != StateExpired || m.Seats() != 0 {
		t.Errorf("after the quarter hour: %s %d", m.State(), m.Seats())
	}
}

func TestRunStopsWithTheContext(t *testing.T) {
	m := openManager(t, t.TempDir(), &fakeClock{t: t0}, &logBuffer{})
	m.tickEvery = time.Millisecond
	done := make(chan struct{})
	ctx, cancel := contextWithCancel()
	go func() { m.Run(ctx); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	_ = io.Discard
}

func contextWithCancel() (ctx context.Context, cancel context.CancelFunc) {
	return context.WithCancel(context.Background())
}
