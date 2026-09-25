package admin

import (
	"sync"
	"time"
)

// SnapshotTTL is how long a status snapshot is served before it is rebuilt
// (ADMIN-API.md §4.7): a client polling at any rate costs the call path
// one copy a second.
const SnapshotTTL = time.Second

// Snapshot serves a value built by fn, rebuilding it at most once per TTL
// and sharing one build among concurrent callers.
type Snapshot[T any] struct {
	// Now is the clock; time.Now unless a test sets it.
	Now   func() time.Time
	mu    sync.Mutex
	ttl   time.Duration
	fn    func() T
	at    time.Time
	value T
	valid bool
}

// NewSnapshot builds a snapshot cache over fn with the default TTL.
func NewSnapshot[T any](fn func() T) *Snapshot[T] {
	return &Snapshot[T]{ttl: SnapshotTTL, Now: time.Now, fn: fn}
}

// Get returns the cached value, rebuilding it when it is older than TTL.
// Concurrent callers wait for the one rebuild rather than each running fn.
func (s *Snapshot[T]) Get() T {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if !s.valid || now.Sub(s.at) >= s.ttl {
		s.value, s.at, s.valid = s.fn(), now, true
	}
	return s.value
}
