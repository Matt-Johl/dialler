package events

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dialler/server/internal/admin"
)

// fixed returns a ring whose clock is a counter, so At is deterministic.
func fixed(n int) *Ring {
	r := New(n)
	t := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time {
		t = t.Add(time.Second)
		return t
	}
	return r
}

func seqs(ev []Event) []uint64 {
	out := make([]uint64, len(ev))
	for i, e := range ev {
		out[i] = e.Seq
	}
	return out
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEmptyRing(t *testing.T) {
	r := fixed(8)
	ev, next, trunc := r.Since(0, 0)
	if len(ev) != 0 || ev == nil || next != 0 || trunc {
		t.Fatalf("empty: got %v next=%d trunc=%v", ev, next, trunc)
	}
	// A client ahead of an empty ring (after a restart) is told to resync.
	ev, next, trunc = r.Since(50, 0)
	if len(ev) != 0 || next != 0 || trunc {
		t.Fatalf("ahead of empty ring: got %v next=%d trunc=%v", ev, next, trunc)
	}
}

func TestOrderingAndFields(t *testing.T) {
	r := fixed(8)
	r.Emit(KindPresence, "dev-a", "201", map[string]any{"client": "app", "online": true})
	r.Emit(KindCallStart, "", "", nil)
	r.Emit(KindLogLevel, "", "", map[string]any{"level": "debug"})

	ev, next, trunc := r.Since(0, 0)
	if !equalSeqs(seqs(ev), []uint64{1, 2, 3}) || next != 3 || trunc {
		t.Fatalf("got %v next=%d trunc=%v", seqs(ev), next, trunc)
	}
	if ev[0].Kind != KindPresence || ev[0].DeviceID != "dev-a" || ev[0].User != "201" || ev[0].Detail["client"] != "app" {
		t.Fatalf("first event lost its fields: %+v", ev[0])
	}
	if !ev[1].At.After(ev[0].At) || !ev[2].At.After(ev[1].At) {
		t.Fatalf("At not assigned from the ring's clock: %v %v %v", ev[0].At, ev[1].At, ev[2].At)
	}
	if ev[1].Detail != nil {
		t.Fatalf("nil detail should stay nil, got %v", ev[1].Detail)
	}
}

func TestWrapAround(t *testing.T) {
	r := fixed(4)
	for i := 0; i < 10; i++ {
		r.Emit(KindPresence, "", "", nil)
	}
	// Only 7..10 are kept.
	ev, next, trunc := r.Since(0, 0)
	if !equalSeqs(seqs(ev), []uint64{7, 8, 9, 10}) || next != 10 || trunc {
		t.Fatalf("after wrap: got %v next=%d trunc=%v", seqs(ev), next, trunc)
	}
	// Keep going so the read has to straddle the end of the buffer.
	r.Emit(KindPresence, "", "", nil) // 11 overwrites 7 at index 2
	ev, next, _ = r.Since(8, 0)
	if !equalSeqs(seqs(ev), []uint64{9, 10, 11}) || next != 11 {
		t.Fatalf("straddling read: got %v next=%d", seqs(ev), next)
	}
	ev, _, _ = r.Since(0, 2)
	if !equalSeqs(seqs(ev), []uint64{8, 9}) {
		t.Fatalf("limited straddling read: got %v", seqs(ev))
	}
}

func TestSinceLimitNextTruncated(t *testing.T) {
	r := fixed(4)
	for i := 0; i < 10; i++ {
		r.Emit(KindPresence, "", "", nil)
	}
	// Kept: 7, 8, 9, 10.
	cases := []struct {
		since uint64
		limit int
		want  []uint64
		next  uint64
		trunc bool
	}{
		{0, 0, []uint64{7, 8, 9, 10}, 10, false}, // from the oldest, never truncated
		{6, 0, []uint64{7, 8, 9, 10}, 10, false}, // exactly the seq before the oldest: nothing lost
		{5, 0, []uint64{7, 8, 9, 10}, 10, true},  // 6 was lost
		{1, 0, []uint64{7, 8, 9, 10}, 10, true},
		{8, 0, []uint64{9, 10}, 10, false},
		{10, 0, []uint64{}, 10, false}, // up to date: next is since
		{12, 0, []uint64{}, 10, false}, // ahead of the ring: next is the newest so it resyncs
		{0, 2, []uint64{7, 8}, 8, false},
		{7, 1, []uint64{8}, 8, false},
		{3, 1, []uint64{7}, 7, true}, // truncated and limited at once
	}
	for _, c := range cases {
		ev, next, trunc := r.Since(c.since, c.limit)
		if !equalSeqs(seqs(ev), c.want) || next != c.next || trunc != c.trunc {
			t.Errorf("Since(%d, %d): got %v next=%d trunc=%v; want %v next=%d trunc=%v",
				c.since, c.limit, seqs(ev), next, trunc, c.want, c.next, c.trunc)
		}
	}
}

func TestLimitDefaultsAndCap(t *testing.T) {
	r := fixed(2000)
	for i := 0; i < 1500; i++ {
		r.Emit(KindPresence, "", "", nil)
	}
	if ev, _, _ := r.Since(0, 0); len(ev) != DefaultLimit {
		t.Fatalf("default limit: got %d", len(ev))
	}
	if ev, _, _ := r.Since(0, -5); len(ev) != DefaultLimit {
		t.Fatalf("negative limit: got %d", len(ev))
	}
	if ev, next, _ := r.Since(0, 5000); len(ev) != MaxLimit || next != MaxLimit {
		t.Fatalf("over max: got %d next=%d", len(ev), next)
	}
}

func TestSinceCopiesOut(t *testing.T) {
	r := fixed(4)
	r.Emit(KindPresence, "dev-a", "", nil)
	ev, _, _ := r.Since(0, 0)
	ev[0].DeviceID = "tampered"
	again, _, _ := r.Since(0, 0)
	if again[0].DeviceID != "dev-a" {
		t.Fatalf("Since handed out the ring's own storage")
	}
}

func TestDroppedUnderHeldLock(t *testing.T) {
	r := fixed(8)
	r.Emit(KindPresence, "", "", nil)

	// A reader (or anyone) holding the lock: the emitter must not wait.
	r.mu.Lock()
	done := make(chan struct{})
	go func() {
		r.Emit(KindPresence, "", "", nil)
		r.Emit(KindPresence, "", "", nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		r.mu.Unlock()
		t.Fatal("Emit blocked on a held lock")
	}
	r.mu.Unlock()

	if got := r.Dropped(); got != 2 {
		t.Fatalf("dropped = %d, want 2", got)
	}
	ev, next, _ := r.Since(0, 0)
	if len(ev) != 1 || next != 1 {
		t.Fatalf("dropped events must not take a seq: got %v next=%d", seqs(ev), next)
	}
	// And the ring works again once the lock is free.
	r.Emit(KindPresence, "", "", nil)
	if _, next, _ := r.Since(0, 0); next != 2 {
		t.Fatalf("after the lock was released next=%d, want 2", next)
	}
}

func TestNewDefault(t *testing.T) {
	if got := len(New(0).buf); got != Default {
		t.Fatalf("New(0) size %d, want %d", got, Default)
	}
	if got := len(New(-1).buf); got != Default {
		t.Fatalf("New(-1) size %d, want %d", got, Default)
	}
}

// ---- handler ---------------------------------------------------------------

func get(t *testing.T, h http.Handler, query string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/events"+query, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %v: %s", err, rec.Body.String())
	}
	return rec, body
}

func TestHandlerShape(t *testing.T) {
	r := fixed(4)
	h := r.Handler()

	rec, body := get(t, h, "")
	if rec.Code != 200 {
		t.Fatalf("empty ring: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type %q", ct)
	}
	if !strings.Contains(rec.Body.String(), `"events":[]`) {
		t.Fatalf("events must be [] not null: %s", rec.Body.String())
	}
	if _, ok := body["truncated"]; ok {
		t.Fatalf("truncated must be omitted when false: %s", rec.Body.String())
	}
	if body["next"] != float64(0) {
		t.Fatalf("next: %v", body["next"])
	}

	for i := 0; i < 6; i++ {
		r.Emit(KindLineState, "dev_7K3M9Q", "204", map[string]any{"state": "refused", "error": "403 Forbidden"})
	}
	rec, body = get(t, h, "?since=1&limit=2")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if body["truncated"] != true || body["next"] != float64(4) {
		t.Fatalf("truncated/next: %s", rec.Body.String())
	}
	evs := body["events"].([]any)
	if len(evs) != 2 {
		t.Fatalf("events: %s", rec.Body.String())
	}
	first := evs[0].(map[string]any)
	if first["seq"] != float64(3) || first["kind"] != KindLineState || first["device_id"] != "dev_7K3M9Q" || first["user"] != "204" {
		t.Fatalf("first event: %v", first)
	}
	if first["detail"].(map[string]any)["state"] != "refused" {
		t.Fatalf("detail: %v", first["detail"])
	}
	if _, err := time.Parse(time.RFC3339Nano, first["at"].(string)); err != nil {
		t.Fatalf("at is not RFC 3339: %v", first["at"])
	}

	// An event with no device, user or detail omits them.
	r2 := fixed(4)
	r2.Emit(KindCodeIssued, "", "", nil)
	rec, _ = get(t, r2.Handler(), "")
	for _, k := range []string{`"device_id"`, `"user"`, `"detail"`} {
		if strings.Contains(rec.Body.String(), k) {
			t.Fatalf("%s should be omitted: %s", k, rec.Body.String())
		}
	}
}

func TestHandlerBadQuery(t *testing.T) {
	h := fixed(4).Handler()
	cases := []struct{ query, field string }{
		{"?since=abc", "since"},
		{"?since=-1", "since"},
		{"?since=1.5", "since"},
		{"?limit=x", "limit"},
		{"?limit=-2", "limit"},
		{"?since=1&limit=", ""}, // empty limit is absent: fine
	}
	for _, c := range cases {
		rec, body := get(t, h, c.query)
		if c.field == "" {
			if rec.Code != 200 {
				t.Errorf("%s: %d %s", c.query, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != 400 || body["error"] != admin.CodeInvalid || body["field"] != c.field {
			t.Errorf("%s: got %d %s; want 400 invalid field %s", c.query, rec.Code, rec.Body.String(), c.field)
		}
	}
	// Over the cap is honoured as the cap, not refused.
	rec, _ := get(t, h, "?limit=99999")
	if rec.Code != 200 {
		t.Fatalf("limit over max: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerMethod(t *testing.T) {
	h := fixed(4).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/events", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("POST: %d Allow=%q %s", rec.Code, rec.Header().Get("Allow"), rec.Body.String())
	}
	var body admin.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error != admin.CodeMethodNotAllowed {
		t.Fatalf("405 envelope: %v %s", err, rec.Body.String())
	}
}

// ---- concurrency -----------------------------------------------------------

func TestConcurrentEmitAndRead(t *testing.T) {
	const writers, each = 8, 10000
	r := New(Default)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The reader polls the way a client would, checking every page is in
	// order and contiguous.
	readerErr := make(chan error, 1)
	go func() {
		var since uint64
		for {
			select {
			case <-stop:
				readerErr <- nil
				return
			default:
			}
			ev, next, _ := r.Since(since, 0)
			for i, e := range ev {
				if i > 0 && e.Seq != ev[i-1].Seq+1 {
					readerErr <- fmt.Errorf("page not contiguous: %d after %d", e.Seq, ev[i-1].Seq)
					return
				}
				if e.Seq <= since {
					readerErr <- fmt.Errorf("seq %d not newer than since %d", e.Seq, since)
					return
				}
			}
			if next < since {
				readerErr <- fmt.Errorf("next %d went backwards from %d", next, since)
				return
			}
			since = next
		}
	}()

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				r.Emit(KindPresence, "dev", "", map[string]any{"w": w, "i": i})
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	if err := <-readerErr; err != nil {
		t.Fatal(err)
	}

	// Every emit was either kept (took a seq) or counted as dropped.
	r.mu.Lock()
	seq := r.seq
	r.mu.Unlock()
	if int64(seq)+r.Dropped() != writers*each {
		t.Fatalf("seq %d + dropped %d != %d emits", seq, r.Dropped(), writers*each)
	}
	t.Logf("kept %d, dropped %d", seq, r.Dropped())
}
