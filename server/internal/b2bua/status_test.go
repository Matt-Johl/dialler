package b2bua

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"dialler/server/internal/pbx"
	"dialler/server/internal/wire"
)

func TestCallsViewAndTracking(t *testing.T) {
	s := &Server{log: slog.Default()}
	a := &callLeg{name: "caller", party: wire.Party{DisplayName: "Matt", URI: "sip:201@dialler"}}
	b := &callLeg{name: "callee", trunk: true, party: wire.Party{URI: "sip:100@asterisk"}}
	c := newBridgedCall(s, s.log, "call-1", a, b)
	s.live.trackBridged(c)
	s.live.trackWaiting("call-2", time.Now().Add(time.Second))

	calls := s.Calls()
	if len(calls) != 2 || calls[0].CallID != "call-1" || calls[1].CallID != "call-2" {
		t.Fatalf("%+v", calls)
	}
	v := calls[0]
	if v.State != "bridged" || v.A == nil || v.A.Leg != "app" || v.A.User != "201" || v.A.Party.DisplayName != "Matt" ||
		v.B == nil || v.B.Leg != "trunk" || v.B.User != "" || v.Stats != nil {
		t.Fatalf("bridged view: %+v a=%+v b=%+v", v, v.A, v.B)
	}
	if calls[1].State != "waiting_wake" {
		t.Fatalf("waiting view: %+v", calls[1])
	}
	c.heldBy = a
	if v := s.Calls()[0]; v.State != "held" || v.HeldBy != "caller" {
		t.Fatalf("held: %+v", v)
	}
	c.heldBy = nil
	c.handing = make(chan struct{})
	if v := s.Calls()[0]; v.State != "handing" {
		t.Fatalf("handing: %+v", v)
	}
	s.live.untrackBridged("call-1")
	s.live.untrackWaiting("call-2")
	if n := len(s.Calls()); n != 0 {
		t.Fatalf("after untrack: %d calls", n)
	}
}

func TestTrunkStatus(t *testing.T) {
	s := &Server{}
	if st := s.TrunkStatus(); st.Configured {
		t.Fatalf("no trunk: %+v", st)
	}
	s.cfg.Trunk = &pbx.Trunk{Host: "asterisk", Port: 5060, Transport: "udp"}
	s.cfg.TrunkQualify = 10 * time.Second
	if st := s.TrunkStatus(); !st.Configured || st.Qualify != "off" {
		t.Fatalf("udp trunk (no qualify): %+v", st)
	}
	s.cfg.Trunk.Transport = "tcp"
	now := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	s.trunk.set(true, nil, now)
	if st := s.TrunkStatus(); st.Qualify != "up" || !st.Since.Equal(now) || st.LastError != "" {
		t.Fatalf("up: %+v", st)
	}
	s.trunk.set(false, errors.New("timeout"), now.Add(time.Minute))
	if st := s.TrunkStatus(); st.Qualify != "down" || !st.Since.Equal(now.Add(time.Minute)) || st.LastError != "timeout" {
		t.Fatalf("down: %+v", st)
	}
	// Same state again does not move since.
	s.trunk.set(false, errors.New("again"), now.Add(2*time.Minute))
	if st := s.TrunkStatus(); !st.Since.Equal(now.Add(time.Minute)) || st.LastError != "again" {
		t.Fatalf("still down: %+v", st)
	}
}

// The qualifier reports each transition, and only transitions.
func TestQualifierReportsState(t *testing.T) {
	var states []bool
	fail := true
	q := &trunkQualifier{
		interval: 5 * time.Millisecond, timeout: time.Second, log: slog.Default(),
		probe: func(context.Context) error {
			if fail {
				return errors.New("no answer")
			}
			return nil
		},
		onDead:  func() {},
		onState: func(up bool, err error) { states = append(states, up) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	fail = false
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	if len(states) != 2 || states[0] || !states[1] {
		t.Fatalf("transitions %v, want [false true]", states)
	}
}
