package b2bua

import (
	"sort"
	"sync"
	"time"
)

// Live state for the admin API (ADMIN-API.md §5.6, §5.8): the registry of
// bridged calls and the trunk's qualify state. Both are read-only views
// copied under a lock of their own, so a status poll never touches a
// call's lock or the SIP stack.

// TrunkStatus is the trunk as the status endpoint reports it.
type TrunkStatus struct {
	Configured bool      `json:"configured"`
	Qualify    string    `json:"qualify,omitempty"` // up, down, off
	Since      time.Time `json:"since,omitzero"`
	LastError  string    `json:"last_error,omitempty"`
}

// CallView is one live call.
type CallView struct {
	CallID string    `json:"call_id"`
	State  string    `json:"state"` // waiting_wake, bridged, held, handing
	Since  time.Time `json:"since"`
	HeldBy string    `json:"held_by,omitempty"`
	A      *LegView  `json:"a,omitempty"`
	B      *LegView  `json:"b,omitempty"`
	Stats  *Stats    `json:"stats,omitempty"`
}

// LegView is one side of a call.
type LegView struct {
	Leg   string `json:"leg"` // app or trunk
	User  string `json:"user,omitempty"`
	Party struct {
		DisplayName string `json:"display_name"`
		URI         string `json:"uri"`
	} `json:"party"`
	Codec string `json:"codec,omitempty"`
	SRTP  bool   `json:"srtp"`
}

// Stats is the relay's counters in each direction.
type Stats struct {
	AToB PumpStats `json:"a_to_b"`
	BToA PumpStats `json:"b_to_a"`
}

// PumpStats is one direction of the relay.
type PumpStats struct {
	Packets      int64     `json:"packets"`
	Lost         int64     `json:"lost"`
	Reordered    int64     `json:"reordered"`
	JitterMs     float64   `json:"jitter_ms"`
	LastPacketAt time.Time `json:"last_packet_at,omitzero"`
}

// liveCalls is the registry of calls the B2BUA is carrying.
type liveCalls struct {
	mu      sync.Mutex
	bridged map[string]*bridgedCall
	waiting map[string]time.Time // call id → since, for calls waiting on a wake
}

func (l *liveCalls) trackBridged(c *bridgedCall) {
	l.mu.Lock()
	if l.bridged == nil {
		l.bridged = map[string]*bridgedCall{}
	}
	l.bridged[c.callID] = c
	l.mu.Unlock()
}

func (l *liveCalls) untrackBridged(callID string) {
	l.mu.Lock()
	delete(l.bridged, callID)
	l.mu.Unlock()
}

func (l *liveCalls) trackWaiting(callID string, since time.Time) {
	l.mu.Lock()
	if l.waiting == nil {
		l.waiting = map[string]time.Time{}
	}
	l.waiting[callID] = since
	l.mu.Unlock()
}

func (l *liveCalls) untrackWaiting(callID string) {
	l.mu.Lock()
	delete(l.waiting, callID)
	l.mu.Unlock()
}

// Calls snapshots every live call, bridged ones first by start time, then
// those waiting on a wake.
func (s *Server) Calls() []CallView {
	s.live.mu.Lock()
	bridged := make([]*bridgedCall, 0, len(s.live.bridged))
	for _, c := range s.live.bridged {
		bridged = append(bridged, c)
	}
	waiting := make([]CallView, 0, len(s.live.waiting))
	for id, since := range s.live.waiting {
		waiting = append(waiting, CallView{CallID: id, State: "waiting_wake", Since: since})
	}
	s.live.mu.Unlock()

	out := make([]CallView, 0, len(bridged)+len(waiting))
	for _, c := range bridged {
		out = append(out, c.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].Since.Before(waiting[j].Since) })
	return append(out, waiting...)
}

// view copies what the call knows under its own lock: fields and atomic
// counters only, no I/O and nothing on the SIP stack.
func (c *bridgedCall) view() CallView {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := CallView{CallID: c.callID, State: "bridged", Since: c.since}
	switch {
	case c.handing != nil:
		v.State = "handing"
	case c.heldBy != nil:
		v.State = "held"
		v.HeldBy = c.heldBy.name
	}
	if c.a != nil {
		v.A = c.a.view()
	}
	if c.b != nil {
		v.B = c.b.view()
	}
	if len(c.pumps) == 2 {
		v.Stats = &Stats{AToB: c.pumps[0].stats(), BToA: c.pumps[1].stats()}
	}
	return v
}

func (l *callLeg) view() *LegView {
	v := &LegView{Leg: "app", Codec: l.codec.Name}
	if l.trunk {
		v.Leg = "trunk"
	} else {
		v.User = uriUser(l.party.URI)
	}
	v.Party.DisplayName, v.Party.URI = l.party.DisplayName, l.party.URI
	if l.sess != nil {
		if m := l.sess.Media(); m != nil {
			v.SRTP = srtpState(m.MediaSession()) == "on"
		}
	}
	return v
}

func (p *pump) stats() PumpStats {
	st := PumpStats{Packets: p.read.Load(), Lost: p.lost.Load(), Reordered: p.reordered.Load(), JitterMs: p.jitterMs()}
	if ms := p.lastReadAt.Load(); ms > 0 {
		st.LastPacketAt = time.UnixMilli(ms)
	}
	return st
}

// trunkState is what the qualifier last found.
type trunkState struct {
	mu      sync.Mutex
	up      bool
	since   time.Time
	lastErr string
}

func (t *trunkState) set(up bool, err error, now time.Time) {
	t.mu.Lock()
	if t.up != up || t.since.IsZero() {
		t.since = now
	}
	t.up = up
	if err != nil {
		t.lastErr = err.Error()
	}
	t.mu.Unlock()
}

// TrunkStatus reports the trunk: absent, qualify off, or up/down as the
// OPTIONS probe last found it.
func (s *Server) TrunkStatus() TrunkStatus {
	if s.cfg.Trunk == nil {
		return TrunkStatus{}
	}
	st := TrunkStatus{Configured: true, Qualify: "off"}
	if s.cfg.TrunkQualify <= 0 || !reliableTransport(s.cfg.Trunk.Transport) {
		return st
	}
	s.trunk.mu.Lock()
	defer s.trunk.mu.Unlock()
	st.Qualify, st.Since, st.LastError = "down", s.trunk.since, s.trunk.lastErr
	if s.trunk.up {
		st.Qualify = "up"
	}
	return st
}
