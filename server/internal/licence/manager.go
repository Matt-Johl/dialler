package licence

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager holds the installed licence for one server: the file, the parsed
// licence, and the state the date gives it. It is safe for concurrent use.
//
// Opening never fails: a missing or unusable file is a state the summary
// reports and the log says once, and the server runs with no seats until
// a licence is installed. Expiry is observed in place by Run's tick, since
// nothing else happens at that moment; it fires OnChange(0) exactly once.
type Manager struct {
	path      string
	installID string
	now       func() time.Time
	log       *slog.Logger
	tickEvery time.Duration

	mu       sync.Mutex
	token    string   // the installed token, "" when none
	lic      *Licence // parsed and bound to this install; nil otherwise
	loadErr  error    // why the stored file is unusable, when it is
	onChange func(seats int)
	// Logging state: transitions, not every tick.
	lastSeats     int
	warnedAt      time.Time
	expiredLogged bool
}

// Open loads the licence at path, if any, for the server whose install id
// is installID. now is the clock (time.Now in production; tests inject).
func Open(path, installID string, now func() time.Time, log *slog.Logger) *Manager {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{path: path, installID: installID, now: now, log: log, tickEvery: time.Minute}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		log.Warn("licence: none installed; no device can enrol or connect until one is (paste it on the admin Licence page)", "install_id", installID)
	case err != nil:
		m.loadErr = fmt.Errorf("read %s: %w", path, err)
		log.Error("licence: stored licence unreadable", "path", path, "err", err)
	default:
		tok := strings.TrimSpace(string(raw))
		lic, err := m.verify(tok)
		if err != nil {
			m.loadErr = err
			log.Error("licence: stored licence is not usable; no device can enrol or connect until one is installed", "path", path, "err", err, "install_id", installID)
		} else {
			m.token, m.lic = tok, &lic
		}
	}
	m.lastSeats = m.seatsLocked(m.now())
	m.logStateLocked(m.now())
	return m
}

// verify parses and binds a token to this install. Expiry is not an error
// here: an expired licence is kept so the summary can say what expired.
func (m *Manager) verify(tok string) (Licence, error) {
	lic, err := Parse(tok)
	if err != nil {
		return Licence{}, err
	}
	if lic.InstallID != m.installID {
		return Licence{}, fmt.Errorf("%w: issued for install id %s, this server is %s", ErrWrongInstall, lic.InstallID, m.installID)
	}
	return lic, nil
}

// OnChange registers the one callback told when the effective seat count
// changes: on install, and once at expiry.
func (m *Manager) OnChange(fn func(seats int)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

// Install verifies and stores a token, replacing whatever was installed.
// A token that does not verify, is for another install id, or has already
// expired is refused and nothing changes, on disk or in memory. The write
// is a temp file renamed into place, 0600.
func (m *Manager) Install(tok string) (Summary, error) {
	tok = strings.TrimSpace(tok)
	lic, err := m.verify(tok)
	if err != nil {
		return m.Summary(0), err
	}
	now := m.now()
	if err := lic.Check(m.installID, now); err != nil {
		return m.Summary(0), err
	}
	m.mu.Lock()
	if tok == m.token {
		m.mu.Unlock()
		return m.Summary(0), nil
	}
	if err := writeFile(m.path, tok+"\n"); err != nil {
		m.mu.Unlock()
		return m.Summary(0), fmt.Errorf("licence: store: %w", err)
	}
	m.token, m.lic, m.loadErr = tok, &lic, nil
	m.expiredLogged, m.warnedAt = false, time.Time{}
	m.log.Info("licence: installed", "id", lic.ID, "customer", lic.Customer, "seats", lic.Seats, "valid_until", lic.ValidUntil.UTC().Format("2006-01-02"))
	m.mu.Unlock()
	m.fire(now)
	return m.Summary(0), nil
}

// writeFile writes atomically: temp file beside the target, then rename.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".licence-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// seatsLocked is the effective seat count now: the licence's while it is in
// force, zero otherwise.
func (m *Manager) seatsLocked(now time.Time) int {
	if m.lic == nil {
		return 0
	}
	if m.lic.StateAt(now) == StateExpired {
		return 0
	}
	return m.lic.Seats
}

// stateLocked is the state now.
func (m *Manager) stateLocked(now time.Time) State {
	switch {
	case m.lic != nil:
		return m.lic.StateAt(now)
	case errors.Is(m.loadErr, ErrWrongInstall):
		return StateWrongInstall
	case m.loadErr != nil:
		return StateInvalid
	}
	return StateMissing
}

// Seats is the effective seat count now.
func (m *Manager) Seats() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seatsLocked(m.now())
}

// State is the licence's state now.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked(m.now())
}

// Token is the installed token for the welcome, or "" when there is no
// licence in force: an expired one is not handed to phones, which would
// only refuse it.
func (m *Manager) Token() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seatsLocked(m.now()) == 0 && (m.lic == nil || m.lic.StateAt(m.now()) == StateExpired) {
		return ""
	}
	return m.token
}

// Summary reports the licence with the given number of seats in use.
func (m *Manager) Summary(seatsUsed int) Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	s := Summary{State: m.stateLocked(now), InstallID: m.installID, SeatsUsed: seatsUsed}
	if m.loadErr != nil {
		s.Error = m.loadErr.Error()
	}
	if m.lic != nil {
		s.ID, s.Customer = m.lic.ID, m.lic.Customer
		s.Seats = m.seatsLocked(now)
		s.IssuedAt, s.ValidUntil = m.lic.IssuedAt, m.lic.ValidUntil
		s.DaysLeft = m.lic.DaysLeft(now)
	}
	return s
}

// Run observes the clock until ctx ends: expiry in place, and the warning
// in the last WarnWindow once a day.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.tickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(m.now())
		}
	}
}

// tick is one observation of the clock.
func (m *Manager) tick(now time.Time) {
	m.mu.Lock()
	m.logStateLocked(now)
	m.mu.Unlock()
	m.fire(now)
}

// fire tells OnChange if the effective seat count has changed since it was
// last told. Called without the lock held, as the callback disconnects
// sessions and drops lines.
func (m *Manager) fire(now time.Time) {
	m.mu.Lock()
	seats := m.seatsLocked(now)
	changed := seats != m.lastSeats
	m.lastSeats = seats
	fn := m.onChange
	m.mu.Unlock()
	if changed && fn != nil {
		fn(seats)
	}
}

// logStateLocked says what needs saying about the date: the warning once a
// day inside the window, the expiry once.
func (m *Manager) logStateLocked(now time.Time) {
	if m.lic == nil {
		return
	}
	switch m.lic.StateAt(now) {
	case StateExpiring:
		if m.warnedAt.IsZero() || now.Sub(m.warnedAt) >= 24*time.Hour {
			m.warnedAt = now
			m.log.Warn("licence: expires soon; renew it on the admin Licence page", "days_left", m.lic.DaysLeft(now), "valid_until", m.lic.ValidUntil.UTC().Format("2006-01-02"), "id", m.lic.ID)
		}
	case StateExpired:
		if !m.expiredLogged {
			m.expiredLogged = true
			m.log.Error("licence: expired; every device is suspended until a licence is installed", "valid_until", m.lic.ValidUntil.UTC().Format("2006-01-02"), "id", m.lic.ID)
		}
	}
}
