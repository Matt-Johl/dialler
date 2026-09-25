// Package loglevel is the one server-wide write in the admin API
// (protocol/ADMIN-API.md §5.10): the process's log level and the SIP trace
// global, read by GET /v1/admin/log and set by PUT /v1/admin/log.
//
// Debug logging and the SIP trace cost the call path CPU (§4.7), so they
// can only be turned on for a bounded time: for_seconds is required for
// them, and the controller reverts to the startup values on its own.
// There is no way to leave them on from the API. Nothing here is
// persisted.
package loglevel

import (
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"dialler/server/internal/admin"
)

// MaxForSeconds is the longest a debug level or SIP trace may be held.
const MaxForSeconds = 3600

// Errors Set returns, so a handler can name the field at fault.
var (
	ErrBadLevel           = errors.New("level must be one of debug, info, warn, error")
	ErrForSecondsRequired = errors.New("for_seconds is required for level debug or sip_trace true")
	ErrForSecondsRange    = errors.New("for_seconds must be between 1 and 3600")
)

// View is the body of GET and PUT /v1/admin/log.
type View struct {
	Level    string     `json:"level"`
	SIPTrace bool       `json:"sip_trace"`
	Until    *time.Time `json:"until"`
	Startup  struct {
		Level    string `json:"level"`
		SIPTrace bool   `json:"sip_trace"`
	} `json:"startup"`
}

// Controller owns the process's log level and the SIP trace switch, with
// the startup values to revert to and the timer that does it.
type Controller struct {
	level    *slog.LevelVar
	setTrace func(bool)
	startup  struct {
		level slog.Level
		trace bool
	}
	now func() time.Time

	mu    sync.Mutex
	trace bool        // the trace switch as last set through here
	until time.Time   // zero when no revert is scheduled
	timer *time.Timer // the pending revert, nil when none
	gen   uint64      // bumped on every Set, so a stale timer does nothing
}

// New returns a controller over level (which main has already set to
// startupLevel) and setTrace (main passes func(on bool) { sip.SIPDebug =
// on }, already applied with startupTrace). It applies nothing itself.
func New(level *slog.LevelVar, setTrace func(bool), startupLevel slog.Level, startupTrace bool) *Controller {
	if setTrace == nil {
		setTrace = func(bool) {}
	}
	c := &Controller{level: level, setTrace: setTrace, now: time.Now, trace: startupTrace}
	c.startup.level = startupLevel
	c.startup.trace = startupTrace
	return c
}

// View reports the current and startup values, and when the current ones
// revert (nil when they hold until restart).
func (c *Controller) View() View {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.viewLocked()
}

func (c *Controller) viewLocked() View {
	var v View
	v.Level = levelName(c.level.Level())
	v.SIPTrace = c.trace
	if !c.until.IsZero() {
		u := c.until
		v.Until = &u
	}
	v.Startup.Level = levelName(c.startup.level)
	v.Startup.SIPTrace = c.startup.trace
	return v
}

// Set applies the §5.10 rules. A nil field is unchanged. level must be one
// of debug, info, warn, error (ErrBadLevel). forSeconds, when given, is 1
// to MaxForSeconds (ErrForSecondsRange) and schedules a revert to the
// startup values after that long, replacing any earlier one; it is
// required (ErrForSecondsRequired) when the resulting level is debug or
// the resulting sip_trace is true. When it is absent for info, warn or
// error any pending revert is cancelled and the values hold until
// restart. Nothing changes on an error.
func (c *Controller) Set(level *string, sipTrace *bool, forSeconds *int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	newLevel := c.level.Level()
	if level != nil {
		l, ok := parseLevel(*level)
		if !ok {
			return ErrBadLevel
		}
		newLevel = l
	}
	newTrace := c.trace
	if sipTrace != nil {
		newTrace = *sipTrace
	}
	if forSeconds != nil && (*forSeconds < 1 || *forSeconds > MaxForSeconds) {
		return ErrForSecondsRange
	}
	if forSeconds == nil && (newLevel == slog.LevelDebug || newTrace) {
		return ErrForSecondsRequired
	}

	c.level.Set(newLevel)
	c.trace = newTrace
	c.setTrace(newTrace)

	c.cancelLocked()
	if forSeconds != nil {
		d := time.Duration(*forSeconds) * time.Second
		c.until = c.now().Add(d)
		gen := c.gen
		c.timer = time.AfterFunc(d, func() { c.expire(gen) })
	}
	return nil
}

// cancelLocked drops any pending revert and moves the generation on, so a
// timer that has already fired but not yet taken the lock does nothing.
func (c *Controller) cancelLocked() {
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.until = time.Time{}
	c.gen++
}

// expire is what the timer runs: a revert, unless a later Set replaced it.
func (c *Controller) expire(gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	c.revertLocked()
}

// revert restores the startup values now and clears any pending revert.
// Tests call it in place of waiting for the timer.
func (c *Controller) revert() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revertLocked()
}

func (c *Controller) revertLocked() {
	c.cancelLocked()
	c.level.Set(c.startup.level)
	c.trace = c.startup.trace
	c.setTrace(c.startup.trace)
}

func levelName(l slog.Level) string {
	switch {
	case l <= slog.LevelDebug:
		return "debug"
	case l <= slog.LevelInfo:
		return "info"
	case l <= slog.LevelWarn:
		return "warn"
	default:
		return "error"
	}
}

func parseLevel(s string) (slog.Level, bool) {
	switch s {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	}
	return 0, false
}

// putBody is the body of PUT /v1/admin/log; a nil field is absent.
type putBody struct {
	Level      *string `json:"level"`
	SIPTrace   *bool   `json:"sip_trace"`
	ForSeconds *int    `json:"for_seconds"`
}

// PutLimit is the PUT body bound (§5.10: 1 KiB).
const PutLimit = 1024

// Handler serves GET and PUT /v1/admin/log (§5.10).
func (c *Controller) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/admin/log", func(w http.ResponseWriter, r *http.Request) {
		admin.WriteJSON(w, http.StatusOK, c.View())
	})
	mux.HandleFunc("PUT /v1/admin/log", func(w http.ResponseWriter, r *http.Request) {
		var body putBody
		if !admin.Decode(w, r, PutLimit, &body) {
			return
		}
		switch err := c.Set(body.Level, body.SIPTrace, body.ForSeconds); {
		case err == nil:
			admin.WriteJSON(w, http.StatusOK, c.View())
		case errors.Is(err, ErrBadLevel):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, err.Error(), "level")
		case errors.Is(err, ErrForSecondsRequired):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, err.Error(), "for_seconds")
		case errors.Is(err, ErrForSecondsRange):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, err.Error(), "for_seconds")
		default:
			admin.WriteError(w, http.StatusBadRequest, admin.CodeInvalid, err.Error())
		}
	})
	return mux
}
