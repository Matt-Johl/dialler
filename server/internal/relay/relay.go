// Package relay is the app-leg media relay (SPEC §4.4 rule 3): every RTP
// stream to or from an app passes through a server-owned UDP port pair, so
// devices behind NAT never need a direct path. Each leg latches to the first
// source address it hears from (symmetric RTP, RFC 4961) and forwards to the
// other leg's latched peer. RTCP is expected on the same port (rtcp-mux).
package relay

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
)

// ErrNoPorts is returned when the configured range is exhausted.
var ErrNoPorts = errors.New("relay: no free ports")

// Relay allocates sessions from a port range on one bind address.
type Relay struct {
	ip       net.IP
	min, max int
	mu       sync.Mutex
	next     int
	inUse    map[int]bool
}

// New creates a relay binding ports [portMin, portMax] on bindIP.
func New(bindIP string, portMin, portMax int) (*Relay, error) {
	ip := net.ParseIP(bindIP)
	if ip == nil {
		return nil, fmt.Errorf("relay: bad bind ip %q", bindIP)
	}
	if portMin <= 0 || portMax < portMin || portMax > 65535 {
		return nil, fmt.Errorf("relay: bad port range %d-%d", portMin, portMax)
	}
	return &Relay{ip: ip, min: portMin, max: portMax, next: portMin, inUse: map[int]bool{}}, nil
}

// Session is one bridged pair of legs.
type Session struct {
	r    *Relay
	A, B *Leg
	once sync.Once
}

// Leg is one side of a session.
type Leg struct {
	conn  *net.UDPConn
	port  int
	peer  atomic.Pointer[net.UDPAddr]
	pkts  atomic.Uint64
	drops atomic.Uint64
}

// Addr is the local address a party should send RTP to.
func (l *Leg) Addr() *net.UDPAddr { return l.conn.LocalAddr().(*net.UDPAddr) }

// Peer is the latched remote address, or nil before the first packet.
func (l *Leg) Peer() *net.UDPAddr { return l.peer.Load() }

// Packets returns packets forwarded from this leg; Drops counts packets
// discarded because the other leg had no peer yet or the source was not the
// latched peer.
func (l *Leg) Packets() uint64 { return l.pkts.Load() }
func (l *Leg) Drops() uint64   { return l.drops.Load() }

// Allocate binds two ports and starts forwarding between them.
func (r *Relay) Allocate() (*Session, error) {
	a, err := r.bind()
	if err != nil {
		return nil, err
	}
	b, err := r.bind()
	if err != nil {
		r.release(a)
		return nil, err
	}
	s := &Session{r: r, A: a, B: b}
	go s.pump(a, b)
	go s.pump(b, a)
	return s, nil
}

func (r *Relay) bind() (*Leg, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	span := r.max - r.min + 1
	for i := 0; i < span; i++ {
		p := r.next
		r.next++
		if r.next > r.max {
			r.next = r.min
		}
		if r.inUse[p] {
			continue
		}
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: r.ip, Port: p})
		if err != nil {
			continue // port taken by something else; try the next
		}
		r.inUse[p] = true
		return &Leg{conn: c, port: p}, nil
	}
	return nil, ErrNoPorts
}

func (r *Relay) release(l *Leg) {
	_ = l.conn.Close()
	r.mu.Lock()
	delete(r.inUse, l.port)
	r.mu.Unlock()
}

// Close stops forwarding and frees both ports.
func (s *Session) Close() {
	s.once.Do(func() {
		s.r.release(s.A)
		s.r.release(s.B)
	})
}

func (s *Session) pump(from, to *Leg) {
	buf := make([]byte, 2048)
	for {
		n, src, err := from.conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		peer := from.peer.Load()
		if peer == nil {
			from.peer.Store(src)
		} else if !peer.IP.Equal(src.IP) || peer.Port != src.Port {
			from.drops.Add(1)
			continue
		}
		dst := to.peer.Load()
		if dst == nil {
			from.drops.Add(1)
			continue
		}
		if _, err := to.conn.WriteToUDP(buf[:n], dst); err == nil {
			from.pkts.Add(1)
		}
	}
}
