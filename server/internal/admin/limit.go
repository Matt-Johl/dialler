package admin

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// The bounds of ADMIN-API.md §4.7. Constants, not flags: they exist so
// that no amount of admin traffic can degrade a call, and an operator has
// no reason to raise them.
const (
	// MaxConnections is how many admin connections are accepted at once;
	// the listener stops accepting beyond it until one closes.
	MaxConnections = 64
	// MaxInflight is how many admin handlers run at once, past auth and
	// the rate limit. On a machine that also runs the relay for every
	// call, this is what bounds admin CPU.
	MaxInflight = 4
	// InflightWait is how long a request queues for a slot before it is
	// answered 503 busy.
	InflightWait = 2 * time.Second
	// PerSourceRate and PerSourceBurst bound one source address, in
	// requests a second.
	PerSourceRate  = 50
	PerSourceBurst = 100
	// GlobalRate and GlobalBurst bound the listener as a whole.
	GlobalRate  = 200
	GlobalBurst = 200

	// Server timeouts (§4.7), applied to both HTTP listeners.
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 30 * time.Second
	WriteTimeout      = 60 * time.Second
	IdleTimeout       = 60 * time.Second
	MaxHeaderBytes    = 16 << 10
)

// Stats counts what the bounds did, for GET /v1/admin/server.
type Stats struct {
	InFlight     atomic.Int64
	RejectedBusy atomic.Int64
	RejectedRate atomic.Int64
}

// StatsView is Stats as the server endpoint reports it.
type StatsView struct {
	InFlight     int64 `json:"in_flight"`
	RejectedBusy int64 `json:"rejected_busy"`
	RejectedRate int64 `json:"rejected_rate"`
}

// View snapshots the counters.
func (s *Stats) View() StatsView {
	return StatsView{InFlight: s.InFlight.Load(), RejectedBusy: s.RejectedBusy.Load(), RejectedRate: s.RejectedRate.Load()}
}

// ConfigureServer applies the §4.7 timeouts to an http.Server.
func ConfigureServer(srv *http.Server) {
	srv.ReadHeaderTimeout = ReadHeaderTimeout
	srv.ReadTimeout = ReadTimeout
	srv.WriteTimeout = WriteTimeout
	srv.IdleTimeout = IdleTimeout
	srv.MaxHeaderBytes = MaxHeaderBytes
}

// ---- connection limit ------------------------------------------------------

// LimitListener returns a listener that accepts at most n connections at
// once. The (n+1)th Accept blocks until one of the n closes. Wrap the raw
// TCP listener, then TLS on top: a closed TLS conn closes its underlying
// conn and releases the slot.
func LimitListener(ln net.Listener, n int) net.Listener {
	return &limitListener{Listener: ln, sem: make(chan struct{}, n)}
}

type limitListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, sem: l.sem}, nil
}

type limitConn struct {
	net.Conn
	sem  chan struct{}
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.sem })
	return err
}

// ---- in-flight cap -----------------------------------------------------------

// Inflight lets at most n requests run at once. A request that cannot get
// a slot within wait is answered 503 busy with Retry-After.
func Inflight(n int, wait time.Duration, st *Stats) func(http.Handler) http.Handler {
	sem := make(chan struct{}, n)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case sem <- struct{}{}:
			default:
				t := time.NewTimer(wait)
				defer t.Stop()
				select {
				case sem <- struct{}{}:
				case <-t.C:
					st.RejectedBusy.Add(1)
					w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())))
					WriteError(w, http.StatusServiceUnavailable, CodeBusy, "too many admin requests in flight; try again")
					return
				case <-r.Context().Done():
					return
				}
			}
			st.InFlight.Add(1)
			defer func() {
				st.InFlight.Add(-1)
				<-sem
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// ---- rate limit --------------------------------------------------------------

// RateLimiter is two token buckets: one per source address and one for the
// listener as a whole. A request is allowed only when both have a token.
type RateLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	sources map[string]*bucket
	global  bucket
	perRate float64
	perCap  float64
	gRate   float64
	gCap    float64
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter builds a limiter with the §4.6 bounds.
func NewRateLimiter() *RateLimiter {
	return newRateLimiter(PerSourceRate, PerSourceBurst, GlobalRate, GlobalBurst, time.Now)
}

func newRateLimiter(perRate, perBurst, gRate, gBurst float64, now func() time.Time) *RateLimiter {
	return &RateLimiter{
		now: now, sources: map[string]*bucket{},
		global:  bucket{tokens: gBurst, last: now()},
		perRate: perRate, perCap: perBurst, gRate: gRate, gCap: gBurst,
	}
}

// Allow reports whether one request from src may proceed now, consuming a
// token from both buckets if so.
func (l *RateLimiter) Allow(src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.sources[src]
	if !ok {
		l.sweepLocked(now)
		b = &bucket{tokens: l.perCap, last: now}
		l.sources[src] = b
	}
	refill(b, now, l.perRate, l.perCap)
	refill(&l.global, now, l.gRate, l.gCap)
	if b.tokens < 1 || l.global.tokens < 1 {
		return false
	}
	b.tokens--
	l.global.tokens--
	return true
}

func refill(b *bucket, now time.Time, rate, capacity float64) {
	if now.After(b.last) {
		b.tokens += rate * now.Sub(b.last).Seconds()
		if b.tokens > capacity {
			b.tokens = capacity
		}
	}
	b.last = now
}

// sweepLocked drops sources idle for a minute so a flood from many
// addresses cannot grow the map without bound. Runs at most once a
// second, when a new source arrives.
func (l *RateLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.swept) < time.Second {
		return
	}
	l.swept = now
	for src, b := range l.sources {
		if now.Sub(b.last) > time.Minute {
			delete(l.sources, src)
		}
	}
}

// RateLimit answers 429 with Retry-After when the limiter refuses.
func RateLimit(l *RateLimiter, st *Stats) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(sourceOf(r)) {
				st.RejectedRate.Add(1)
				w.Header().Set("Retry-After", "1")
				WriteError(w, http.StatusTooManyRequests, CodeRateLimited, "too many admin requests; slow down")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func sourceOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
