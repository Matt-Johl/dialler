// Package events is the admin API's operational event ring
// (protocol/ADMIN-API.md §5.9): a bounded in-memory record of the last N
// things that happened, lost on restart, read by GET /v1/admin/events.
//
// Everything in it is already a log line; the ring makes it a read. It
// replaces no logging.
package events

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"dialler/server/internal/admin"
)

// Event kinds, exactly as §5.9 lists them. Detail keys are per kind and
// are the emitter's concern.
const (
	KindPresence           = "presence"
	KindSIPRegister        = "sip_register"
	KindSIPUnregister      = "sip_unregister"
	KindLineState          = "line_state"
	KindTrunkState         = "trunk_state"
	KindCallStart          = "call_start"
	KindCallEnd            = "call_end"
	KindWakeSent           = "wake_sent"
	KindWakeAck            = "wake_ack"
	KindCodeIssued         = "code_issued"
	KindCodeClaimed        = "code_claimed"
	KindCodeCancelled      = "code_cancelled"
	KindDeviceCreated      = "device_created"
	KindDeviceRevoked      = "device_revoked"
	KindDevicePurged       = "device_purged"
	KindDescriptionChanged = "description_changed"
	KindConfigChanged      = "config_changed"
	KindLineChanged        = "line_changed"
	KindLineRemoved        = "line_removed"
	KindDirectoryChanged   = "directory_changed"
	KindLogLevel           = "log_level"
)

// Event is one entry of the ring, as the API serialises it.
type Event struct {
	Seq      uint64         `json:"seq"`
	At       time.Time      `json:"at"`
	Kind     string         `json:"kind"`
	DeviceID string         `json:"device_id,omitempty"`
	User     string         `json:"user,omitempty"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// Default is the ring size §5.9 fixes: the last 4,096 events.
const Default = 4096

// Read limits (§5.9): limit defaults to DefaultLimit and is capped at
// MaxLimit.
const (
	DefaultLimit = 200
	MaxLimit     = 1000
)

// Ring holds the last N events. Emit is O(1) under the ring's own mutex
// and never waits for it (§4.7): the emitters are the call path, and a
// reader copying the ring out must never stall a REGISTER, an INVITE or a
// wake. So Emit takes the lock with TryLock, and when a reader holds it
// the event is dropped and counted instead. Dropped is reported by
// GET /v1/admin/server.
type Ring struct {
	mu      sync.Mutex
	buf     []Event // fixed at len n once the ring has filled
	start   int     // index of the oldest event when count > 0
	count   int     // events held, at most len(buf)
	seq     uint64  // seq of the newest event; 0 before the first
	now     func() time.Time
	dropped atomic.Int64
}

// New returns a ring keeping the last n events; n <= 0 means Default.
func New(n int) *Ring {
	if n <= 0 {
		n = Default
	}
	return &Ring{buf: make([]Event, n), now: time.Now}
}

// Emit appends one event. It assigns the seq and the time, and returns
// at once whether or not the event was kept: if the lock is held (a reader
// is copying the ring out, or another emitter is mid-append) the event is
// dropped and Dropped is incremented. An emitter on the call path never
// waits (§4.7). detail is kept by reference; the caller does not reuse it.
func (r *Ring) Emit(kind, deviceID, user string, detail map[string]any) {
	if !r.mu.TryLock() {
		r.dropped.Add(1)
		return
	}
	r.seq++
	e := Event{Seq: r.seq, At: r.now(), Kind: kind, DeviceID: deviceID, User: user, Detail: detail}
	n := len(r.buf)
	if r.count < n {
		r.buf[(r.start+r.count)%n] = e
		r.count++
	} else {
		r.buf[r.start] = e
		r.start = (r.start + 1) % n
	}
	r.mu.Unlock()
}

// Dropped is the number of events Emit could not append because the ring
// was locked at that moment.
func (r *Ring) Dropped() int64 { return r.dropped.Load() }

// Since returns the events with Seq > since, oldest first, at most limit
// of them (limit <= 0 means DefaultLimit; over MaxLimit is MaxLimit).
//
// next is the Seq of the last event returned, so a client polls with it;
// when nothing is returned it is the newest seq the ring holds (which is
// since itself when the client is up to date, and lower than since after a
// restart, so the client resyncs instead of waiting forever). truncated is
// true when since is older than the oldest kept event by more than one,
// meaning events between since and the oldest kept were lost; since == 0
// asks for the oldest kept and is never truncated.
//
// The copy out is the only work under the lock.
func (r *Ring) Since(since uint64, limit int) (events []Event, next uint64, truncated bool) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next = r.seq
	if r.count == 0 {
		return []Event{}, next, false
	}
	n := len(r.buf)
	oldest := r.buf[r.start].Seq
	if since != 0 && since+1 < oldest {
		truncated = true
	}
	// The first index to return: seqs are contiguous in the ring, so the
	// event with seq s sits at (start + (s - oldest)) % n.
	skip := 0
	if since >= oldest {
		skip = int(since - oldest + 1)
	}
	avail := r.count - skip
	if avail <= 0 {
		return []Event{}, next, truncated
	}
	if avail > limit {
		avail = limit
	}
	events = make([]Event, avail)
	from := (r.start + skip) % n
	if from+avail <= n {
		copy(events, r.buf[from:from+avail])
	} else {
		k := copy(events, r.buf[from:])
		copy(events[k:], r.buf[:avail-k])
	}
	next = events[avail-1].Seq
	return events, next, truncated
}

// page is the body of GET /v1/admin/events.
type page struct {
	Next      uint64  `json:"next"`
	Events    []Event `json:"events"`
	Truncated bool    `json:"truncated,omitempty"`
}

// Handler serves GET ?since=<seq>&limit=<n> (§5.9) on any path; the mount
// point is the caller's. A since or limit that is not a non-negative
// integer is 400 invalid naming the parameter. Any other method is 405.
func (r *Ring) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			admin.WriteError(w, http.StatusMethodNotAllowed, admin.CodeMethodNotAllowed, "method not allowed")
			return
		}
		q := req.URL.Query()
		var since uint64
		if s := q.Get("since"); s != "" {
			v, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, "since must be a non-negative integer", "since")
				return
			}
			since = v
		}
		limit := 0
		if s := q.Get("limit"); s != "" {
			v, err := strconv.Atoi(s)
			if err != nil || v < 0 {
				admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, "limit must be a non-negative integer", "limit")
				return
			}
			limit = v
		}
		events, next, truncated := r.Since(since, limit)
		admin.WriteJSON(w, http.StatusOK, page{Next: next, Events: events, Truncated: truncated})
	})
}
