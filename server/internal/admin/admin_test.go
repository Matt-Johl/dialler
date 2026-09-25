package admin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// The listener test needs a real socket, which the build sandbox refuses;
// it runs on a developer machine and in the harness image.
func TestLimitListenerHoldsTheConnectionPastTheCap(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen in this sandbox:", err)
	}
	ln := LimitListener(raw, 2)
	defer ln.Close()

	accepted := make(chan net.Conn, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c1, c2 := dial(), dial()
	defer c1.Close()
	defer c2.Close()
	s1 := <-accepted
	s2 := <-accepted
	c3 := dial()
	defer c3.Close()
	select {
	case <-accepted:
		t.Fatal("third connection accepted past the cap of two")
	case <-time.After(150 * time.Millisecond):
	}
	s1.Close()
	select {
	case s3 := <-accepted:
		s3.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("third connection never accepted after a slot was freed")
	}
	s2.Close()
}

func do(h http.Handler, method, path, token, from string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if from != "" {
		req.RemoteAddr = from + ":1234"
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorBody {
	t.Helper()
	var b ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("not a JSON error body: %q", rec.Body.String())
	}
	return b
}

func TestInflightAnswersBusyAfterTheWait(t *testing.T) {
	var st Stats
	release := make(chan struct{})
	entered := make(chan struct{})
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	})
	h := Inflight(1, 50*time.Millisecond, &st)(slow)

	first := make(chan int, 1)
	go func() { first <- do(h, "GET", "/", "", "").Code }()
	<-entered
	if got := st.InFlight.Load(); got != 1 {
		t.Fatalf("in flight = %d, want 1", got)
	}
	rec := do(h, "GET", "/", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second request: status %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("503 without Retry-After")
	}
	if b := errorBody(t, rec); b.Error != CodeBusy {
		t.Fatalf("error code %q, want %q", b.Error, CodeBusy)
	}
	if st.RejectedBusy.Load() != 1 {
		t.Fatalf("rejected_busy = %d, want 1", st.RejectedBusy.Load())
	}
	close(release)
	if c := <-first; c != http.StatusOK {
		t.Fatalf("first request completed with %d", c)
	}
	if got := st.InFlight.Load(); got != 0 {
		t.Fatalf("in flight after completion = %d, want 0", got)
	}
}

func TestInflightQueuesWithinTheWait(t *testing.T) {
	var st Stats
	h := Inflight(1, time.Second, &st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
	}))
	var wg sync.WaitGroup
	codes := make([]int, 3)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = do(h, "GET", "/", "", "").Code
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d got %d; three 30 ms requests fit in a one-second wait", i, c)
		}
	}
}

func TestRateLimiterPerSourceAndGlobal(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	l := newRateLimiter(10, 3, 100, 5, clock)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("a: request %d refused within the burst", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("a: fourth request allowed past a burst of three")
	}
	// Another source has its own bucket, but the global bucket (5) has 2 left.
	if !l.Allow("b") || !l.Allow("b") {
		t.Fatal("b: refused within its own burst and the global one")
	}
	if l.Allow("b") {
		t.Fatal("b: allowed past the global burst")
	}
	// Time passes: a refills at 10/s, global at 100/s.
	now = now.Add(200 * time.Millisecond)
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("a: two tokens should have refilled in 200 ms at 10/s")
	}
	if l.Allow("a") {
		t.Fatal("a: a third token should not have refilled")
	}
}

func TestRateLimiterSweepsIdleSources(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }
	l := newRateLimiter(10, 3, 1000, 1000, clock)
	for i := 0; i < 100; i++ {
		l.Allow(fmt.Sprintf("10.0.0.%d", i))
	}
	now = now.Add(2 * time.Minute)
	l.Allow("fresh")
	if n := len(l.sources); n != 1 {
		t.Fatalf("sources after the sweep = %d, want 1", n)
	}
}

func TestRateLimitMiddlewareAnswers429(t *testing.T) {
	var st Stats
	l := newRateLimiter(1, 1, 100, 100, time.Now)
	h := RateLimit(l, &st)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	if rec := do(h, "GET", "/", "", "10.0.0.1"); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := do(h, "GET", "/", "", "10.0.0.1")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("second: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if b := errorBody(t, rec); b.Error != CodeRateLimited {
		t.Fatalf("code %q", b.Error)
	}
	if rec := do(h, "GET", "/", "", "10.0.0.2"); rec.Code != 200 {
		t.Fatalf("another source: %d", rec.Code)
	}
	if st.RejectedRate.Load() != 1 {
		t.Fatalf("rejected_rate = %d", st.RejectedRate.Load())
	}
}

func TestBearerGuardAnswersJSONAndWhoami(t *testing.T) {
	var st Stats
	api := http.NewServeMux()
	api.Handle("GET /v1/admin/whoami", Whoami())
	h := Chain("secret", NewRateLimiter(), &st, api)

	rec := do(h, "GET", "/v1/admin/whoami", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d, want 401", rec.Code)
	}
	if b := errorBody(t, rec); b.Error != CodeUnauthorized || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("401 body %+v content-type %q", b, rec.Header().Get("Content-Type"))
	}
	rec = do(h, "GET", "/v1/admin/whoami", "secret", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("whoami: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(h, "GET", "/v1/admin/whoami", "wrong", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", rec.Code)
	}
	// The guard runs before routing: an unknown path without a token is
	// 401, not 404, so the API's shape is not enumerable unauthenticated.
	if rec := do(h, "GET", "/v1/admin/nope", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown path without token: %d, want 401", rec.Code)
	}
}

// The mux's own 404 and 405 come out as the envelope too.
func TestMuxErrorsAreJSON(t *testing.T) {
	var st Stats
	api := http.NewServeMux()
	api.Handle("GET /v1/admin/whoami", Whoami())
	api.HandleFunc("GET /v1/admin/plain", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	h := Chain("secret", NewRateLimiter(), &st, api)

	rec := do(h, "GET", "/v1/admin/nope", "secret", "")
	if rec.Code != 404 || errorBody(t, rec).Error != CodeNotFound {
		t.Fatalf("unknown path: %d %s", rec.Code, rec.Body)
	}
	rec = do(h, "DELETE", "/v1/admin/whoami", "secret", "")
	if rec.Code != 405 || errorBody(t, rec).Error != CodeMethodNotAllowed || rec.Header().Get("Allow") == "" {
		t.Fatalf("wrong method: %d %s Allow=%q", rec.Code, rec.Body, rec.Header().Get("Allow"))
	}
	rec = do(h, "GET", "/v1/admin/plain", "secret", "")
	if rec.Code != 404 || errorBody(t, rec).Error != CodeNotFound {
		t.Fatalf("http.NotFound from a handler: %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/v1/admin/whoami", "secret", ""); rec.Code != 200 || rec.Body.String() != "{\"ok\":true}\n" {
		t.Fatalf("a success body must pass through untouched: %d %q", rec.Code, rec.Body)
	}
}

func TestEmptyTokenRefusesEverything(t *testing.T) {
	h := Bearer("")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	if rec := do(h, "GET", "/v1/admin/whoami", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty token: %d, want 401", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	rec := do(Healthz(), "GET", "/healthz", "", "")
	if rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

// A flood against the whole chain from eight sources: every response is
// 200, 429 or 503, none hangs, and the counters account for every refusal.
func TestFloodNeverHangs(t *testing.T) {
	var st Stats
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	h := Chain("secret", NewRateLimiter(), &st, api)

	const n = 1500
	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code := do(h, "GET", "/x", "secret", fmt.Sprintf("10.0.0.%d", i%8)).Code
			mu.Lock()
			counts[code]++
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if d := time.Since(start); d > 8*time.Second {
		t.Fatalf("flood took %v", d)
	}
	for code := range counts {
		switch code {
		case 200, 429, 503:
		default:
			t.Fatalf("unexpected outcome %d in %v", code, counts)
		}
	}
	if counts[200] == 0 {
		t.Fatalf("nothing succeeded: %v", counts)
	}
	if int(st.RejectedRate.Load()) != counts[429] || int(st.RejectedBusy.Load()) != counts[503] {
		t.Fatalf("counters %+v do not match outcomes %v", st.View(), counts)
	}
	if st.InFlight.Load() != 0 {
		t.Fatal("in-flight did not return to zero")
	}
}
