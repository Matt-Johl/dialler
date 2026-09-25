// Package pbxline keeps this server registered to a PBX as one third-party
// SIP device per app device (SPEC §6 item 3c).
//
// The server registers, never the app. A phone holding the PBX credential
// would be the registered endpoint, so the PBX would send its INVITE to a
// contact that stops existing the moment iOS suspends the app — the problem
// this product exists to solve (§2). So the lines here are held around the
// clock whatever the phones are doing, the INVITE always lands on a live
// element, and the wake path is untouched.
//
// This package owns the *timing and the state* of those registrations: when
// to refresh, how long to back off, when to give up. The SIP itself is
// behind the Registrar seam, as trunkQualifier's probe is (b2bua/qualify.go),
// so the machine that decides all of that is testable without a SIP stack.
package pbxline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"slices"
	"strings"
	"sync"
	"time"
)

// Line is one device's line on the PBX: the local extension it is known by
// here, the directory number the PBX knows it as, and the digest credential
// the PBX issued for it.
type Line struct {
	// User is the local extension — the device's registry user, what the
	// app registers to us as and what routing resolves.
	User string
	// DN is the directory number on the PBX. Normally identical to User;
	// they differ only where a deployment's CUCM numbering does not match
	// the extension the app dials as.
	DN string
	// DigestUser is the PBX-side credential username (on CUCM, the End
	// User named as the device's Digest User — not the DN).
	DigestUser string
	// Secret is that credential's password. It never leaves this server:
	// it is not pushed to a device and no admin read returns it.
	Secret string
}

// normalised fills DN from User, which is the usual case.
func (l Line) normalised() Line {
	if l.DN == "" {
		l.DN = l.User
	}
	return l
}

// sameCredential reports whether two lines would produce the same
// registration. A write that changes nothing must not disturb a live line.
func (l Line) sameCredential(o Line) bool {
	return l.DN == o.DN && l.DigestUser == o.DigestUser && l.Secret == o.Secret
}

// State is what a line's registration is doing.
type State string

const (
	// StatePending is a line whose first REGISTER has not been sent yet.
	StatePending State = "pending"
	// StateRegistered is a line the PBX has accepted and which is being
	// refreshed before it expires.
	StateRegistered State = "registered"
	// StateRetrying is a line whose REGISTER failed for a reason that may
	// pass — the PBX is down, the network is out, a 5xx.
	StateRetrying State = "retrying"
	// StateRefused is a line the PBX rejected on its merits: a wrong
	// credential, an unknown line. Retrying cannot fix it, so the loop
	// stops until an administrator changes the credential.
	StateRefused State = "refused"
)

// Status is one line's registration as the admin API reports it. It never
// carries the secret.
type Status struct {
	User       string    `json:"user"`
	DN         string    `json:"dn"`
	DigestUser string    `json:"digest_user"`
	State      State     `json:"state"`
	Since      time.Time `json:"since"`
	// ExpiresAt is when the registration we hold lapses; zero unless
	// registered.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Realm is what the PBX challenged with, kept for diagnostics: a realm
	// that is not what the operator expected is the usual first sign of a
	// credential provisioned against the wrong cluster.
	Realm string `json:"realm,omitempty"`
	// Error is why the line is retrying or refused.
	Error string `json:"error,omitempty"`
	// Attempts is how many consecutive failures the line has seen.
	Attempts int `json:"attempts,omitempty"`
}

// Registration is what the PBX granted.
type Registration struct {
	// Expiry is the lifetime the PBX gave us, which may be shorter than
	// the one we asked for; the refresh follows it, not our request.
	Expiry time.Duration
	// Realm is the realm it challenged with, empty if it did not challenge.
	Realm string
}

// Registrar sends one REGISTER for a line and reports what came back. It
// owns the SIP and the digest exchange; this package owns everything else.
//
// A refusal on the credential's merits — a challenge answered and refused
// again, a line the PBX does not have — must be reported as *Refused so the
// loop stops instead of hammering the exchange. Every other error is taken
// as transient.
type Registrar interface {
	Register(ctx context.Context, l Line, expiry time.Duration) (Registration, error)
	// Unregister drops the line's contact (Expires: 0). Best effort, at
	// shutdown and when a line is removed, so the PBX does not ring a
	// contact we have stopped listening on.
	Unregister(ctx context.Context, l Line) error
}

// Refused is a final refusal: the credential or the line is wrong and
// retrying cannot change that.
type Refused struct {
	Status int
	Reason string
}

func (e *Refused) Error() string {
	return fmt.Sprintf("pbx refused the registration: %d %s", e.Status, e.Reason)
}

// Config is the manager's wiring. Every duration has a working default, and
// the two sources of non-determinism — the clock and the jitter — are
// injected so the loop can be tested without waiting on either.
type Config struct {
	Registrar Registrar
	// Expiry is the registration lifetime we ask for (default 1 hour).
	Expiry time.Duration
	// MinRefresh floors the refresh interval, so a PBX granting a very
	// short expiry cannot turn the fleet into a REGISTER storm.
	MinRefresh time.Duration
	// BackoffBase and BackoffCap bound the retry interval after a
	// transient failure (defaults 5 s and 5 min).
	BackoffBase, BackoffCap time.Duration
	// StartSpread is the window the first REGISTER of each line is
	// scattered over, so a fleet coming up does not stampede one PBX node
	// (default 5 s).
	StartSpread time.Duration
	// UnregisterTimeout bounds the best-effort Expires:0 at shutdown.
	UnregisterTimeout time.Duration
	Log               *slog.Logger
	// Now and Rand are seams for tests. Rand returns a value in [0,1).
	Now  func() time.Time
	Rand func() float64
	// OnState, if set, is told each line state change (for the event ring).
	OnState func(Status)
}

func (c *Config) applyDefaults() {
	if c.Expiry <= 0 {
		c.Expiry = time.Hour
	}
	if c.MinRefresh <= 0 {
		c.MinRefresh = 5 * time.Second
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 5 * time.Second
	}
	if c.BackoffCap <= 0 {
		c.BackoffCap = 5 * time.Minute
	}
	if c.StartSpread < 0 {
		c.StartSpread = 0
	}
	if c.UnregisterTimeout <= 0 {
		c.UnregisterTimeout = 2 * time.Second
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
}

// Manager holds the fleet's lines. It is safe for concurrent use, and its
// read-only accessors are safe on a nil receiver so a caller in trunk mode
// (where no manager exists) needs no special case.
type Manager struct {
	cfg Config

	mu      sync.Mutex
	lines   map[string]*line // by Line.User
	ctx     context.Context  // non-nil once started
	started bool
	wg      sync.WaitGroup
}

// line is one registration's goroutine and the state it publishes.
type line struct {
	mu     sync.Mutex
	cur    Line
	status Status

	// wake carries "the credential changed, register again now"; buffered
	// so a write never blocks on a loop that is mid-REGISTER.
	wake   chan struct{}
	cancel context.CancelFunc
}

// New builds a manager. Nothing registers until Start.
func New(cfg Config) *Manager {
	cfg.applyDefaults()
	return &Manager{cfg: cfg, lines: map[string]*line{}}
}

// Start begins registering every line configured so far, and every line
// added from here on. It returns immediately; Stop waits for the loops.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return
	}
	m.started, m.ctx = true, ctx
	for _, l := range m.lines {
		m.startLocked(l)
	}
	m.cfg.Log.Info("pbx lines: registering", "lines", len(m.lines), "expiry", m.cfg.Expiry)
}

// Stop cancels every line's loop and waits for it, dropping each contact on
// the way out so the PBX does not ring a line we have stopped serving.
func (m *Manager) Stop() {
	m.mu.Lock()
	running := m.started
	lines := make([]Line, 0, len(m.lines))
	for _, l := range m.lines {
		if l.cancel != nil {
			l.cancel()
		}
		lines = append(lines, l.snapshot())
	}
	m.started = false
	m.mu.Unlock()
	m.wg.Wait()
	if !running {
		return
	}
	// Drop the bindings. Without this the exchange goes on sending calls to
	// a contact nothing is listening on until the registration expires —
	// up to an hour of callers ringing out for no reason. Concurrently and
	// best effort: each is bounded, and a PBX that has itself gone away
	// must not hold up the shutdown once per line.
	var wg sync.WaitGroup
	for _, l := range lines {
		wg.Add(1)
		go func(l Line) {
			defer wg.Done()
			m.unregister(l)
		}(l)
	}
	wg.Wait()
}

// Set replaces the configured lines wholesale — the device store's view at
// start-up. Lines that are unchanged are left running untouched: re-setting
// an identical fleet must not drop a single registration.
func (m *Manager) Set(lines []Line) {
	keep := make(map[string]bool, len(lines))
	for _, l := range lines {
		keep[l.User] = true
		m.Put(l)
	}
	m.mu.Lock()
	var gone []string
	for user := range m.lines {
		if !keep[user] {
			gone = append(gone, user)
		}
	}
	m.mu.Unlock()
	for _, user := range gone {
		m.Delete(user)
	}
}

// Put adds a line or replaces one. A write that changes nothing is a no-op;
// a write that changes the credential re-registers that line at once and
// clears a refusal, which is how an administrator fixes a wrong password.
func (m *Manager) Put(in Line) {
	l := in.normalised()
	if l.User == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ex, ok := m.lines[l.User]; ok {
		ex.mu.Lock()
		unchanged := ex.cur.sameCredential(l)
		ex.cur = l
		ex.status.DN, ex.status.DigestUser = l.DN, l.DigestUser
		ex.mu.Unlock()
		if !unchanged {
			ex.signal()
		}
		return
	}
	n := &line{cur: l, wake: make(chan struct{}, 1)}
	n.status = Status{User: l.User, DN: l.DN, DigestUser: l.DigestUser, State: StatePending, Since: m.cfg.Now()}
	m.lines[l.User] = n
	if m.started {
		m.startLocked(n)
	}
}

// Delete stops a line and drops its contact on the PBX.
func (m *Manager) Delete(user string) {
	m.mu.Lock()
	l, ok := m.lines[user]
	if ok {
		delete(m.lines, user)
	}
	started := m.started
	m.mu.Unlock()
	if !ok {
		return
	}
	if l.cancel != nil {
		l.cancel()
	}
	if !started {
		return
	}
	go m.unregister(l.snapshot())
}

// startLocked launches one line's loop. m.mu is held.
func (m *Manager) startLocked(l *line) {
	if l.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(m.ctx)
	l.cancel = cancel
	m.wg.Add(1)
	go m.run(ctx, l)
}

// Credentials returns the digest credential to answer a PBX challenge for a
// call placed on this user's behalf. Nil-safe: trunk mode has no manager.
func (m *Manager) Credentials(user string) (digestUser, secret string, ok bool) {
	if m == nil {
		return "", "", false
	}
	m.mu.Lock()
	l, found := m.lines[user]
	m.mu.Unlock()
	if !found {
		return "", "", false
	}
	c := l.snapshot()
	if c.DigestUser == "" {
		return "", "", false
	}
	return c.DigestUser, c.Secret, true
}

// Number returns the directory number a call from this user must present as
// its calling party. Nil-safe.
func (m *Manager) Number(user string) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	l, ok := m.lines[user]
	m.mu.Unlock()
	if !ok {
		return "", false
	}
	return l.snapshot().DN, true
}

// Statuses reports every line, ordered by user. Nil-safe.
func (m *Manager) Statuses() []Status {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	out := make([]Status, 0, len(m.lines))
	for _, l := range m.lines {
		out = append(out, l.state())
	}
	m.mu.Unlock()
	sortStatuses(out)
	return out
}

// Status reports one line. Nil-safe.
func (m *Manager) Status(user string) (Status, bool) {
	if m == nil {
		return Status{}, false
	}
	m.mu.Lock()
	l, ok := m.lines[user]
	m.mu.Unlock()
	if !ok {
		return Status{}, false
	}
	return l.state(), true
}

// ---- the loop ---------------------------------------------------------------

// run keeps one line registered until ctx ends.
//
// Logging follows the qualifier's discipline: transitions, not every
// failure. A PBX that is down says so once and once more when it returns.
func (m *Manager) run(ctx context.Context, l *line) {
	defer m.wg.Done()

	log := m.cfg.Log.With("line", l.snapshot().User)
	// Scatter the fleet's first REGISTER: fifty lines coming up together
	// would otherwise arrive at one PBX node in the same millisecond.
	if d := time.Duration(m.cfg.Rand() * float64(m.cfg.StartSpread)); d > 0 {
		if !m.wait(ctx, l, d) {
			return
		}
	}

	attempt := 0
	for {
		cur := l.snapshot()
		reg, err := m.cfg.Registrar.Register(ctx, cur, m.cfg.Expiry)
		if ctx.Err() != nil {
			return
		}

		// errors.As rather than a type switch: a Registrar is free to wrap
		// its refusal in context, and a refusal that went unrecognised
		// would be retried for ever against an exchange that has already
		// said no.
		var refused *Refused
		var wait time.Duration
		switch {
		case err == nil:
			granted := reg.Expiry
			if granted <= 0 {
				granted = m.cfg.Expiry
			}
			was := l.state().State
			l.registered(m.cfg.Now(), granted, reg.Realm)
			m.stateChanged(l)
			if was != StateRegistered {
				log.Info("pbx line: registered", "dn", cur.DN, "expiry", granted, "realm", reg.Realm)
			}
			attempt = 0
			wait = refreshAfter(granted, m.cfg.MinRefresh)

		case errors.As(err, &refused):
			l.refused(m.cfg.Now(), refused)
			m.stateChanged(l)
			log.Warn("pbx line: refused; not retrying until the credential changes",
				"dn", cur.DN, "digest_user", cur.DigestUser, "status", refused.Status, "reason", refused.Reason)
			// Nothing but an administrator can help now. Park until the
			// credential changes (Put signals) or the server stops.
			if !m.waitForChange(ctx, l) {
				return
			}
			attempt = 0
			continue

		default:
			attempt++
			if l.state().State != StateRetrying {
				log.Warn("pbx line: registration failed; retrying", "dn", cur.DN, "err", err)
			}
			l.retrying(m.cfg.Now(), err, attempt)
			m.stateChanged(l)
			wait = m.jittered(backoff(attempt, m.cfg.BackoffBase, m.cfg.BackoffCap))
		}

		if !m.wait(ctx, l, wait) {
			return
		}
	}
}

// wait sleeps for d, returning false if the server stopped. A credential
// change cuts the wait short: a corrected password should take effect now,
// not at the next refresh.
func (m *Manager) wait(ctx context.Context, l *line, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-l.wake:
		return true
	case <-t.C:
		return true
	}
}

// waitForChange blocks a refused line until its credential changes.
func (m *Manager) waitForChange(ctx context.Context, l *line) bool {
	select {
	case <-ctx.Done():
		return false
	case <-l.wake:
		return true
	}
}

// unregister drops one line's contact, bounded and best effort.
func (m *Manager) unregister(l Line) {
	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.UnregisterTimeout)
	defer cancel()
	if err := m.cfg.Registrar.Unregister(ctx, l); err != nil {
		m.cfg.Log.Debug("pbx line: unregister failed", "line", l.User, "err", err)
	}
}

// jittered spreads a retry by ±20 % so lines that failed together do not
// retry together.
func (m *Manager) jittered(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*m.cfg.Rand()))
}

// refreshAfter is when to re-register given the lifetime the PBX granted:
// three quarters of it, leaving a full quarter to retry in before the
// contact lapses, and never below the floor.
func refreshAfter(granted, floor time.Duration) time.Duration {
	d := granted * 3 / 4
	if d < floor {
		return floor
	}
	return d
}

// backoff is the interval before retry number n (1-based): the base,
// doubling, capped.
func backoff(attempt int, base, limit time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	if d > limit {
		return limit
	}
	return d
}

// ---- line state -------------------------------------------------------------

func (l *line) snapshot() Line {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}

func (l *line) state() Status {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status
}

// signal asks the loop to register again now.
func (l *line) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *line) registered(now time.Time, granted time.Duration, realm string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status.State != StateRegistered {
		l.status.Since = now
	}
	l.status.State = StateRegistered
	l.status.ExpiresAt = now.Add(granted)
	l.status.Error, l.status.Attempts = "", 0
	if realm != "" {
		l.status.Realm = realm
	}
}

func (l *line) retrying(now time.Time, err error, attempt int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status.State != StateRetrying {
		l.status.Since = now
	}
	l.status.State = StateRetrying
	l.status.ExpiresAt = time.Time{}
	l.status.Error, l.status.Attempts = err.Error(), attempt
}

func (l *line) refused(now time.Time, err *Refused) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status.State != StateRefused {
		l.status.Since = now
	}
	l.status.State = StateRefused
	l.status.ExpiresAt = time.Time{}
	l.status.Error = err.Error()
}

// stateChanged tells Config.OnState, if set, a line's new state.
func (m *Manager) stateChanged(l *line) {
	if m.cfg.OnState != nil {
		m.cfg.OnState(l.state())
	}
}

// sortStatuses orders by user, so the admin API and the logs are stable.
func sortStatuses(s []Status) {
	slices.SortFunc(s, func(a, b Status) int { return strings.Compare(a.User, b.User) })
}
