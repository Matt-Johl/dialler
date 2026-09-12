package b2bua

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"dialler/server/internal/registry"
	"dialler/server/internal/routing"
	"dialler/server/internal/wire"
)

// What the caller hears when the callee refuses or cannot be reached.
//
// The app rejects a ringing call with 486 Busy Here (baresip's default, the
// same as a desk phone); the caller must get that back, not 480, which the
// PBX and the calling phone render as "no response". A device that was only
// woken (app killed) refuses through wake_ack{decline}; that must end the
// caller's ringing at once with 486 rather than at the 30 s ring timeout.

type fakeWaker struct {
	delivered int
	cancels   []wire.CancelReason
	forgotten []string
}

func (f *fakeWaker) Wake(string, wire.Wake) int                  { return f.delivered }
func (f *fakeWaker) CancelWake(_, _ string, r wire.CancelReason) { f.cancels = append(f.cancels, r) }
func (f *fakeWaker) ForgetWake(_, callID string)                 { f.forgotten = append(f.forgotten, callID) }

func newWakeServer(t *testing.T, w Waker) *Server {
	t.Helper()
	reg := registry.New(nil)
	s, err := New(Config{TLS: &tls.Config{}, ExternalHost: "10.0.0.1",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, reg, routing.New(reg, nil, nil), w)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Registered-app path: the callee's own refusal (486/600/603) is relayed to
// the caller unchanged; everything else, including a callee that never
// answered, is 480.
func TestCallerStatusRelaysRefusalsOnly(t *testing.T) {
	resp := func(code int, reason string) *sip.Response {
		return &sip.Response{StatusCode: code, Reason: reason}
	}
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"486 by value", sipgo.ErrDialogResponse{Res: resp(486, "Busy Here")}, 486},
		{"603 by pointer", &sipgo.ErrDialogResponse{Res: resp(603, "Decline")}, 603},
		{"600 wrapped", fmt.Errorf("invite: %w", sipgo.ErrDialogResponse{Res: resp(600, "Busy Everywhere")}), 600},
		{"404 is not a refusal", sipgo.ErrDialogResponse{Res: resp(404, "Not Found")}, 480},
		{"408 timeout", sipgo.ErrDialogResponse{Res: resp(408, "Request Timeout")}, 480},
		{"context cancelled", context.Canceled, 480},
		{"plain error", errors.New("dial tcp: connection refused"), 480},
	}
	for _, c := range cases {
		if code, _ := callerStatus(c.err); code != c.code {
			t.Errorf("%s: caller told %d, want %d", c.name, code, c.code)
		}
	}
}

// Wake path: a decline (or busy) wake_ack ends the wait for a registration
// immediately with that cause — wakeAndWaitFrom then answers the caller 486
// — and sends no wake_cancel, because the device already stopped ringing.
func TestRefusalAckEndsTheWakeWaitImmediately(t *testing.T) {
	for _, tc := range []struct {
		action wire.WakeAction
		want   error
	}{
		{wire.WakeDecline, errWakeDeclined},
		{wire.WakeBusy, errWakeBusy},
	} {
		w := &fakeWaker{delivered: 1}
		s := newWakeServer(t, w)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // the ring timeout
		done := make(chan error, 1)
		go func() {
			_, err := s.wakeAndWaitFrom(ctx, s.log, nil, "call-1", nil, registry.Endpoint{User: "201", DeviceID: "dev-a"})
			done <- err
		}()
		// The ack is a no-op until the wait is registered, so keep acking.
		err := ackUntilDone(t, s, "call-1", tc.action, done)
		cancel()
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: wait ended with %v, want %v", tc.action, err, tc.want)
		}
		if len(w.cancels) != 0 {
			t.Fatalf("%s: sent wake_cancel %v, but the device already refused", tc.action, w.cancels)
		}
	}
}

func ackUntilDone(t *testing.T, s *Server, callID string, action wire.WakeAction, done <-chan error) error {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.HandleWakeAck("dev", wire.WakeAck{CallID: callID, Action: action})
		select {
		case err := <-done:
			return err
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the wait did not end on the ack", action)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// will_answer is not a refusal: the wait continues (here until the ring
// timeout, which then cancels the wake as before).
func TestWillAnswerAckDoesNotEndTheWait(t *testing.T) {
	w := &fakeWaker{delivered: 1}
	s := newWakeServer(t, w)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.wakeAndWaitFrom(ctx, s.log, nil, "call-2", nil, registry.Endpoint{User: "202", DeviceID: "dev-b"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	s.HandleWakeAck("dev-b", wire.WakeAck{CallID: "call-2", Action: wire.WakeWillAnswer})
	err := <-done
	if errors.Is(err, errWakeDeclined) || errors.Is(err, errWakeBusy) {
		t.Fatalf("will_answer ended the wait as a refusal: %v", err)
	}
	if len(w.cancels) != 1 {
		t.Fatalf("expected one wake_cancel after the ring timeout, got %v", w.cancels)
	}
	// The wait's cancel entry must not leak once the call is over.
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	if _, still := s.waiting["call-2"]; still {
		t.Fatal("finished call still tracked as waiting")
	}
}
