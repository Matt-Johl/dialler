package relay

import (
	"errors"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireUDP skips the test when the environment forbids binding sockets
// (e.g. a build sandbox). The relay is the one component whose tests
// genuinely need real UDP; everything else runs socket-free.
func requireUDP(t *testing.T) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		if errors.Is(err, syscall.EPERM) || strings.Contains(err.Error(), "not permitted") {
			t.Skipf("UDP bind forbidden in this environment: %v", err)
		}
		t.Fatal(err)
	}
	_ = c.Close()
}

func phone(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func expectPacket(t *testing.T, c *net.UDPConn, want string) {
	t.Helper()
	buf := make([]byte, 64)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("waiting for %q: %v", want, err)
	}
	if string(buf[:n]) != want {
		t.Fatalf("got %q want %q", buf[:n], want)
	}
}

func expectSilence(t *testing.T, c *net.UDPConn) {
	t.Helper()
	buf := make([]byte, 64)
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _, err := c.ReadFromUDP(buf); err == nil {
		t.Fatalf("unexpected packet %q", buf[:n])
	}
}

func TestBridgeAndLatch(t *testing.T) {
	requireUDP(t)
	r, err := New("127.0.0.1", 40000, 40010)
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	a, b := phone(t), phone(t)

	// A speaks first: latches leg A, but B has no peer yet → dropped.
	_, _ = a.WriteToUDP([]byte("a1"), s.A.Addr())
	expectSilence(t, b)

	// B speaks: latches leg B and reaches A.
	_, _ = b.WriteToUDP([]byte("b1"), s.B.Addr())
	expectPacket(t, a, "b1")

	// Now both directions flow.
	_, _ = a.WriteToUDP([]byte("a2"), s.A.Addr())
	expectPacket(t, b, "a2")

	// An interloper sending to leg A is ignored.
	x := phone(t)
	_, _ = x.WriteToUDP([]byte("evil"), s.A.Addr())
	expectSilence(t, b)

	if s.A.Peer().Port != a.LocalAddr().(*net.UDPAddr).Port || s.B.Peer().Port != b.LocalAddr().(*net.UDPAddr).Port {
		t.Fatal("latched peers wrong")
	}
	deadline := time.Now().Add(time.Second)
	for s.A.Drops() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if s.A.Packets() != 1 || s.A.Drops() != 2 || s.B.Packets() != 1 {
		t.Fatalf("stats A=%d/%d B=%d/%d", s.A.Packets(), s.A.Drops(), s.B.Packets(), s.B.Drops())
	}
}

func TestPortExhaustionAndReuse(t *testing.T) {
	requireUDP(t)
	r, err := New("127.0.0.1", 40020, 40023) // room for exactly two sessions
	if err != nil {
		t.Fatal(err)
	}
	s1, err := r.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := r.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Allocate(); err != ErrNoPorts {
		t.Fatalf("want ErrNoPorts, got %v", err)
	}
	s1.Close()
	s1.Close() // idempotent
	s3, err := r.Allocate()
	if err != nil {
		t.Fatalf("ports not reused after Close: %v", err)
	}
	s3.Close()
	s2.Close()
}

func TestBadConfig(t *testing.T) {
	if _, err := New("not-an-ip", 1, 2); err == nil {
		t.Fatal("bad ip accepted")
	}
	if _, err := New("127.0.0.1", 10, 5); err == nil {
		t.Fatal("inverted range accepted")
	}
}
