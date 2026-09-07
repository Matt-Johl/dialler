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
