package loglevel

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"dialler/server/internal/admin"
)

// fixture is a controller over a fresh LevelVar and a recorded trace
// switch, with a frozen clock.
type fixture struct {
	c     *Controller
	lv    *slog.LevelVar
	trace bool
	calls int
	now   time.Time
}

func newFixture(t *testing.T, level slog.Level, trace bool) *fixture {
	t.Helper()
	f := &fixture{lv: new(slog.LevelVar), trace: trace, now: time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)}
	f.lv.Set(level)
	f.c = New(f.lv, func(on bool) { f.trace = on; f.calls++ }, level, trace)
	f.c.now = func() time.Time { return f.now }
	t.Cleanup(func() {
		f.c.mu.Lock()
		f.c.cancelLocked()
		f.c.mu.Unlock()
	})
	return f
}

func str(s string) *string { return &s }
func boolp(b bool) *bool   { return &b }
func intp(i int) *int      { return &i }

func TestViewAtStart(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	v := f.c.View()
	if v.Level != "info" || v.SIPTrace || v.Until != nil || v.Startup.Level != "info" || v.Startup.SIPTrace {
		t.Fatalf("view: %+v", v)
	}
	if f.calls != 0 {
		t.Fatalf("New must not touch the trace switch, called %d times", f.calls)
	}

	// The startup values are whatever main started with.
	g := newFixture(t, slog.LevelWarn, true)
	v = g.c.View()
	if v.Level != "warn" || !v.SIPTrace || v.Startup.Level != "warn" || !v.Startup.SIPTrace {
		t.Fatalf("view: %+v", v)
	}
}

func TestDebugRequiresForSeconds(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	if err := f.c.Set(str("debug"), nil, nil); err != ErrForSecondsRequired {
		t.Fatalf("err = %v", err)
	}
	if f.lv.Level() != slog.LevelInfo || f.calls != 0 {
		t.Fatalf("a refused Set changed something: level=%v calls=%d", f.lv.Level(), f.calls)
	}
}

func TestDebugWithForSecondsAndRevert(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	if err := f.c.Set(str("debug"), nil, intp(600)); err != nil {
		t.Fatal(err)
	}
	if f.lv.Level() != slog.LevelDebug {
		t.Fatalf("LevelVar not applied: %v", f.lv.Level())
	}
	v := f.c.View()
	want := f.now.Add(600 * time.Second)
	if v.Level != "debug" || v.Until == nil || !v.Until.Equal(want) {
		t.Fatalf("view: %+v", v)
	}
	if f.c.timer == nil {
		t.Fatal("no revert timer scheduled")
	}

	f.c.revert()
	v = f.c.View()
	if f.lv.Level() != slog.LevelInfo || v.Level != "info" || v.Until != nil || v.SIPTrace {
		t.Fatalf("after revert: level=%v view=%+v", f.lv.Level(), v)
	}
	if f.c.timer != nil {
		t.Fatal("revert left a timer behind")
	}
}

func TestTimerFires(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	if err := f.c.Set(str("debug"), boolp(true), intp(1)); err != nil {
		t.Fatal(err)
	}
	// The timer runs on the real clock: fire it now rather than sleeping.
	f.c.mu.Lock()
	timer, gen := f.c.timer, f.c.gen
	f.c.mu.Unlock()
	timer.Stop()
	f.c.expire(gen)
	if f.lv.Level() != slog.LevelInfo || f.trace {
		t.Fatalf("expire did not revert: level=%v trace=%v", f.lv.Level(), f.trace)
	}
	if f.c.View().Until != nil {
		t.Fatal("until not cleared")
	}
}

func TestStaleTimerDoesNothing(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	if err := f.c.Set(str("debug"), nil, intp(10)); err != nil {
		t.Fatal(err)
	}
	f.c.mu.Lock()
	first := f.c.gen
	f.c.mu.Unlock()
	// A second Set replaces the timer; the first one's expiry must not
	// undo it.
	if err := f.c.Set(str("warn"), nil, intp(20)); err != nil {
		t.Fatal(err)
	}
	f.c.expire(first)
	v := f.c.View()
	if v.Level != "warn" || v.Until == nil || !v.Until.Equal(f.now.Add(20*time.Second)) {
		t.Fatalf("stale expiry changed state: %+v", v)
	}
}

func TestWarnWithoutForSecondsClearsUntil(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	if err := f.c.Set(str("debug"), nil, intp(600)); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Set(str("warn"), nil, nil); err != nil {
		t.Fatal(err)
	}
	v := f.c.View()
	if v.Level != "warn" || v.Until != nil || f.lv.Level() != slog.LevelWarn {
		t.Fatalf("view: %+v level=%v", v, f.lv.Level())
	}
	if f.c.timer != nil {
		t.Fatal("timer not cleared")
	}
	// It holds until restart: the startup values are untouched.
	if v.Startup.Level != "info" {
		t.Fatalf("startup changed: %+v", v.Startup)
	}
}

func TestSIPTraceRequiresForSeconds(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	if err := f.c.Set(nil, boolp(true), nil); err != ErrForSecondsRequired {
		t.Fatalf("err = %v", err)
	}
	if f.trace || f.calls != 0 {
		t.Fatal("refused Set touched the trace switch")
	}
	if err := f.c.Set(nil, boolp(true), intp(30)); err != nil {
		t.Fatal(err)
	}
	if !f.trace || f.calls != 1 {
		t.Fatalf("trace switch: on=%v calls=%d", f.trace, f.calls)
	}
	v := f.c.View()
	if !v.SIPTrace || v.Level != "info" || v.Until == nil {
		t.Fatalf("view: %+v", v)
	}
	// Turning it off needs no deadline and clears the one that was set.
	if err := f.c.Set(nil, boolp(false), nil); err != nil {
		t.Fatal(err)
	}
	if f.trace || f.c.View().Until != nil {
		t.Fatalf("trace off: on=%v view=%+v", f.trace, f.c.View())
	}
}

func TestUnchangedFieldsStillCount(t *testing.T) {
	// Debug is on with a deadline; a Set that names neither field still
	// resolves to debug and so still needs for_seconds.
	f := newFixture(t, slog.LevelInfo, false)
	if err := f.c.Set(str("debug"), nil, intp(60)); err != nil {
		t.Fatal(err)
	}
	if err := f.c.Set(nil, nil, nil); err != ErrForSecondsRequired {
		t.Fatalf("err = %v", err)
	}
	// Extending is fine and replaces the deadline.
	if err := f.c.Set(nil, nil, intp(120)); err != nil {
		t.Fatal(err)
	}
	if v := f.c.View(); v.Level != "debug" || !v.Until.Equal(f.now.Add(120*time.Second)) {
		t.Fatalf("view: %+v", v)
	}
}

func TestForSecondsRange(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	for _, n := range []int{3601, 0, -1, 1 << 30} {
		if err := f.c.Set(str("debug"), nil, intp(n)); err != ErrForSecondsRange {
			t.Errorf("for_seconds %d: err = %v", n, err)
		}
	}
	if err := f.c.Set(str("debug"), nil, intp(3600)); err != nil {
		t.Fatalf("3600: %v", err)
	}
	// The range is checked even for a level that does not need it.
	if err := f.c.Set(str("info"), nil, intp(3601)); err != ErrForSecondsRange {
		t.Fatalf("info with 3601: err = %v", err)
	}
}

func TestBadLevel(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	for _, s := range []string{"DEBUG", "warning", "trace", "", "Info"} {
		if err := f.c.Set(str(s), nil, intp(10)); err != ErrBadLevel {
			t.Errorf("level %q: err = %v", s, err)
		}
	}
	if f.lv.Level() != slog.LevelInfo {
		t.Fatal("bad level changed the LevelVar")
	}
}

// ---- handler ---------------------------------------------------------------

func do(t *testing.T, h http.Handler, method, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "/v1/admin/log", nil)
	} else {
		req = httptest.NewRequest(method, "/v1/admin/log", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rec.Body.String())
	}
	return rec, out
}

func TestHandlerGet(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	rec, body := do(t, f.c.Handler(), http.MethodGet, "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	want := `{"level":"info","sip_trace":false,"until":null,"startup":{"level":"info","sip_trace":false}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
	_ = body
}

func TestHandlerPut(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	h := f.c.Handler()

	rec, body := do(t, h, http.MethodPut, `{"level":"debug"}`)
	if rec.Code != 400 || body["error"] != admin.CodeMissing || body["field"] != "for_seconds" {
		t.Fatalf("debug without for_seconds: %d %s", rec.Code, rec.Body.String())
	}

	rec, body = do(t, h, http.MethodPut, `{"level":"debug","for_seconds":600}`)
	if rec.Code != 200 || body["level"] != "debug" || body["until"] == nil {
		t.Fatalf("debug with 600: %d %s", rec.Code, rec.Body.String())
	}
	if f.lv.Level() != slog.LevelDebug {
		t.Fatalf("LevelVar: %v", f.lv.Level())
	}
	until, err := time.Parse(time.RFC3339Nano, body["until"].(string))
	if err != nil || !until.Equal(f.now.Add(600*time.Second)) {
		t.Fatalf("until: %v %v", body["until"], err)
	}

	rec, body = do(t, h, http.MethodPut, `{"level":"warn"}`)
	if rec.Code != 200 || body["level"] != "warn" || body["until"] != nil {
		t.Fatalf("warn: %d %s", rec.Code, rec.Body.String())
	}

	rec, body = do(t, h, http.MethodPut, `{"sip_trace":true}`)
	if rec.Code != 400 || body["error"] != admin.CodeMissing || body["field"] != "for_seconds" {
		t.Fatalf("sip_trace without for_seconds: %d %s", rec.Code, rec.Body.String())
	}

	rec, body = do(t, h, http.MethodPut, `{"sip_trace":true,"for_seconds":3601}`)
	if rec.Code != 400 || body["error"] != admin.CodeInvalid || body["field"] != "for_seconds" {
		t.Fatalf("3601: %d %s", rec.Code, rec.Body.String())
	}

	rec, body = do(t, h, http.MethodPut, `{"level":"verbose","for_seconds":10}`)
	if rec.Code != 400 || body["error"] != admin.CodeInvalid || body["field"] != "level" {
		t.Fatalf("bad level: %d %s", rec.Code, rec.Body.String())
	}

	rec, body = do(t, h, http.MethodPut, `{"level":"info","colour":"red"}`)
	if rec.Code != 400 || body["error"] != admin.CodeUnknownField || body["field"] != "colour" {
		t.Fatalf("unknown field: %d %s", rec.Code, rec.Body.String())
	}

	rec, body = do(t, h, http.MethodPut, `{"for_seconds":"600"}`)
	if rec.Code != 400 || body["error"] != admin.CodeInvalid || body["field"] != "for_seconds" {
		t.Fatalf("wrong type: %d %s", rec.Code, rec.Body.String())
	}

	rec, _ = do(t, h, http.MethodPut, `{"level":"info","for_seconds":1,"pad":"`+strings.Repeat("x", PutLimit)+`"}`)
	if rec.Code != 413 {
		t.Fatalf("over 1 KiB: %d %s", rec.Code, rec.Body.String())
	}

	// The last good state still stands after all those refusals.
	if v := f.c.View(); v.Level != "warn" || v.SIPTrace || v.Until != nil {
		t.Fatalf("refusals changed state: %+v", v)
	}
}

func TestHandlerMethod(t *testing.T) {
	f := newFixture(t, slog.LevelInfo, false)
	req := httptest.NewRequest(http.MethodDelete, "/v1/admin/log", nil)
	rec := httptest.NewRecorder()
	f.c.Handler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("DELETE: %d", rec.Code)
	}
}
