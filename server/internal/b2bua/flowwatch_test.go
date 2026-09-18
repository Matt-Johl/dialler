package b2bua

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchFlowFiresOnceWhenTheFlowDies(t *testing.T) {
	var alive atomic.Bool
	alive.Store(true)
	var gone atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		watchFlow(ctx, 5*time.Millisecond, alive.Load, func() { gone.Add(1) })
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	if gone.Load() != 0 {
		t.Fatal("onGone ran while the flow was alive")
	}
	alive.Store(false)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the watch did not stop after the flow died")
	}
	if gone.Load() != 1 {
		t.Fatalf("onGone ran %d times, want exactly once", gone.Load())
	}
}

func TestWatchFlowStopsWithItsContext(t *testing.T) {
	var gone atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchFlow(ctx, 5*time.Millisecond, func() bool { return true }, func() { gone.Add(1) })
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the watch outlived its context")
	}
	if gone.Load() != 0 {
		t.Fatal("onGone ran on a context cancel; only a dead flow may trigger it")
	}
}
