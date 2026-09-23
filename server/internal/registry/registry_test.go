package registry

import (
	"context"
	"testing"
	"time"
)

func TestWaitRegistered(t *testing.T) {
	r := New(nil)
	r.Provision("201", "dev1")

	// Times out while nobody registers.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	if _, err := r.WaitRegistered(ctx, "201"); err != context.DeadlineExceeded {
		t.Fatalf("want deadline, got %v", err)
	}
	cancel()

	// Wakes when the registration arrives.
	got := make(chan Endpoint, 1)
	go func() {
		ep, err := r.WaitRegistered(context.Background(), "201")
		if err != nil {
			t.Error(err)
		}
		got <- ep
	}()
	time.Sleep(10 * time.Millisecond)
	if _, err := r.Register("201", "<sip:201@10.0.0.5:5061;transport=tls>", time.Minute); err != nil {
		t.Fatal(err)
	}
	select {
	case ep := <-got:
		if ep.Contact == "" {
			t.Fatalf("woken without contact: %+v", ep)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitRegistered did not wake on Register")
	}

	// Already registered: returns at once.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if ep, err := r.WaitRegistered(ctx, "201"); err != nil || ep.Contact == "" {
		t.Fatalf("immediate return: %+v %v", ep, err)
	}
}

// A phone that re-registers from a new address while its INVITE is in
// flight must be noticed; a refresh from the same route must not.
func TestWaitRouteChange(t *testing.T) {
	r := New(nil)
	r.Provision("201", "dev1")
	old := "sip:201@10.18.0.204:54962;transport=tls"
	if _, err := r.Register("201", old, time.Minute); err != nil {
		t.Fatal(err)
	}

	// A refresh on the same route is not a change: the wait runs out.
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = r.Register("201", old, time.Minute)
	}()
	if _, err := r.WaitRouteChange(ctx, "201", old); err != context.DeadlineExceeded {
		t.Fatalf("same-route refresh: want deadline, got %v", err)
	}
	cancel()

	// A registration from elsewhere ends the wait with the new route.
	got := make(chan Endpoint, 1)
	go func() {
		ep, err := r.WaitRouteChange(context.Background(), "201", old)
		if err != nil {
			t.Error(err)
		}
		got <- ep
	}()
	time.Sleep(10 * time.Millisecond)
	moved := "sip:201@10.18.0.111:55043;transport=tls"
	if _, err := r.Register("201", moved, time.Minute); err != nil {
		t.Fatal(err)
	}
	select {
	case ep := <-got:
		if ep.Contact != moved {
			t.Fatalf("woken with %q, want %q", ep.Contact, moved)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitRouteChange did not wake on the new registration")
	}

	// Already moved: returns at once.
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if ep, err := r.WaitRouteChange(ctx, "201", old); err != nil || ep.Contact != moved {
		t.Fatalf("immediate return: %+v %v", ep, err)
	}
	// No waiter left behind by the timed-out wait.
	r.mu.Lock()
	n := len(r.waiters["201"])
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d waiter(s) leaked", n)
	}
}

func TestProvisionRegisterExpire(t *testing.T) {
	now := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	r := New(func() time.Time { return now })

	if _, err := r.Register("201", "sip:201@10.0.0.5:5061;transport=tls", time.Minute); err != ErrUnknownUser {
		t.Fatalf("want ErrUnknownUser, got %v", err)
	}

	r.Provision("201", "dev1")
	ep, ok := r.Lookup("201")
	if !ok || ep.DeviceID != "dev1" || ep.Contact != "" || r.Registered("201") {
		t.Fatalf("after provision: %+v ok=%v", ep, ok)
	}
	if ep, ok := r.LookupDevice("dev1"); !ok || ep.User != "201" {
		t.Fatalf("LookupDevice: %+v %v", ep, ok)
	}

	if _, err := r.Register("201", "sip:201@10.0.0.5:5061;transport=tls", time.Minute); err != nil {
		t.Fatal(err)
	}
	if !r.Registered("201") {
		t.Fatal("should be registered")
	}

	now = now.Add(61 * time.Second)
	if r.Registered("201") {
		t.Fatal("registration should have expired")
	}
	if ep, _ := r.Lookup("201"); ep.Contact != "" || !ep.Expires.IsZero() {
		t.Fatalf("expired lookup must clear contact: %+v", ep)
	}

	now = now.Add(-61 * time.Second)
	r.Unregister("201")
	if r.Registered("201") {
		t.Fatal("unregister failed")
	}

	// Rebinding the device keeps the user and moves the device index.
	r.Provision("201", "dev2")
	if _, ok := r.LookupDevice("dev1"); ok {
		t.Fatal("old device still indexed")
	}
	if ep, ok := r.LookupDevice("dev2"); !ok || ep.User != "201" {
		t.Fatal("new device not indexed")
	}

	r.Provision("100", "")
	if got := r.Users(); len(got) != 2 || got[0] != "100" || got[1] != "201" {
		t.Fatalf("Users = %v", got)
	}
	r.Deprovision("201")
	if _, ok := r.Lookup("201"); ok {
		t.Fatal("deprovision failed")
	}
	if _, ok := r.LookupDevice("dev2"); ok {
		t.Fatal("deprovision left device index")
	}
}

func TestUnregisterRouteComparesFirst(t *testing.T) {
	now := time.Date(2026, 9, 23, 7, 11, 0, 0, time.UTC)
	r := New(func() time.Time { return now })
	r.Provision("201", "dev-a")

	const flowA = "sip:201-0x11c280fd0@10.18.0.204:65309;transport=tls"
	const flowB = "sip:201-0x11c280fd0@10.18.0.204:65401;transport=tls"

	if r.UnregisterRoute("201", flowA) {
		t.Fatal("unregistered a user that holds no binding")
	}
	if _, err := r.Register("201", flowA, time.Minute); err != nil {
		t.Fatal(err)
	}
	// The phone re-registered on a new connection while the watcher was
	// still deciding that the old one was dead. Acting on that stale
	// verdict must not drop the binding that replaced it.
	if _, err := r.Register("201", flowB, time.Minute); err != nil {
		t.Fatal(err)
	}
	if r.UnregisterRoute("201", flowA) {
		t.Fatal("stale route verdict dropped the newer binding")
	}
	if ep, _ := r.Lookup("201"); ep.Contact != flowB {
		t.Fatalf("binding should still be the new flow, got %q", ep.Contact)
	}
	// The verdict that does match clears it.
	if !r.UnregisterRoute("201", flowB) {
		t.Fatal("matching route was not cleared")
	}
	if r.Registered("201") {
		t.Fatal("binding survived its flow")
	}
	if r.UnregisterRoute("201", flowB) {
		t.Fatal("second clear reported work it did not do")
	}
}

func TestBindingsListsOnlyLiveRegistrations(t *testing.T) {
	now := time.Date(2026, 9, 23, 7, 11, 0, 0, time.UTC)
	r := New(func() time.Time { return now })
	r.Provision("201", "dev-a")
	r.Provision("202", "dev-b")
	r.Provision("203", "dev-c") // never registers

	if got := r.Bindings(); len(got) != 0 {
		t.Fatalf("want no bindings, got %+v", got)
	}
	if _, err := r.Register("202", "sip:202@10.0.0.6:5061;transport=tls", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register("201", "sip:201@10.0.0.5:5061;transport=tls", time.Minute); err != nil {
		t.Fatal(err)
	}
	got := r.Bindings()
	if len(got) != 2 || got[0].User != "201" || got[1].User != "202" {
		t.Fatalf("want 201,202 sorted, got %+v", got)
	}

	// An expired registration is not a flow worth sweeping.
	now = now.Add(61 * time.Second)
	if got := r.Bindings(); len(got) != 0 {
		t.Fatalf("expired bindings still listed: %+v", got)
	}
}
