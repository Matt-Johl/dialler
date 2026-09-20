package b2bua

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// The qualifier drops the trunk's pooled connection when an OPTIONS goes
// unanswered, on every probe while it stays that way, never while the PBX
// answers — and a probe that runs past its timeout counts as unanswered.
func TestTrunkQualifierDropsTheConnectionWhileOptionsGoUnanswered(t *testing.T) {
	var answer atomic.Bool // whether the fake PBX answers
	answer.Store(true)
	var probes, drops atomic.Int32
	q := &trunkQualifier{
		interval: 10 * time.Millisecond,
		timeout:  20 * time.Millisecond,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		probe: func(ctx context.Context) error {
			probes.Add(1)
			if answer.Load() {
				return nil
			}
			<-ctx.Done() // the dead connection: nothing ever comes back
			return ctx.Err()
		},
		onDead: func() { drops.Add(1) },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.run(ctx)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (probes=%d drops=%d)", what, probes.Load(), drops.Load())
			}
			time.Sleep(time.Millisecond)
		}
	}

	waitFor("a few answered probes", func() bool { return probes.Load() >= 3 })
	if drops.Load() != 0 {
		t.Fatalf("connection dropped %d time(s) while the PBX was answering", drops.Load())
	}

	answer.Store(false) // the connection goes silently dead
	waitFor("the first drop", func() bool { return drops.Load() >= 1 })
	waitFor("repeated drops while dead", func() bool { return drops.Load() >= 3 })

	answer.Store(true) // the PBX answers again on a fresh connection
	n := drops.Load()
	waitFor("answered probes after recovery", func() bool { return probes.Load() >= n+5 })
	time.Sleep(50 * time.Millisecond)
	if drops.Load() > n+1 { // at most the one probe already in flight at recovery
		t.Fatalf("kept dropping the connection after the PBX answered again: %d → %d", n, drops.Load())
	}
}

// A probe error other than the timeout (the send itself failing) is a dead
// connection too.
func TestTrunkQualifierTreatsASendErrorAsDead(t *testing.T) {
	var drops atomic.Int32
	q := &trunkQualifier{
		interval: 5 * time.Millisecond,
		timeout:  50 * time.Millisecond,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		probe:    func(context.Context) error { return errors.New("write: broken pipe") },
		onDead:   func() { drops.Add(1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	q.run(ctx)
	if drops.Load() == 0 {
		t.Fatal("a failing send never dropped the connection")
	}
}

func TestReliableTransport(t *testing.T) {
	for tr, want := range map[string]bool{"tls": true, "TLS": true, "tcp": true, "udp": false, "": false} {
		if got := reliableTransport(tr); got != want {
			t.Errorf("reliableTransport(%q) = %v, want %v", tr, got, want)
		}
	}
}
