// Package gateway is the signal gateway: it terminates client wire-protocol
// connections (foreground app socket and LPC extension socket), authenticates
// them, keeps a per-device session table, and fans wakes out to every live
// connection for a device. It knows nothing about SIP; the call controller
// calls Wake/CancelWake and listens for acks.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"dialler/server/internal/wire"
)

// Authenticator validates a device credential presented in Hello.
type Authenticator interface {
	Authenticate(ctx context.Context, deviceID, token string) (bool, error)
}

// Config tunes a Gateway. Zero values pick the PROTOCOL.md defaults.
type Config struct {
	Heartbeat        time.Duration    // advertised in Welcome; default 25s
	HandshakeTimeout time.Duration    // time allowed for the Hello frame; default 5s
	WriteTimeout     time.Duration    // per-frame write deadline; default 5s
	Now              func() time.Time // clock; default time.Now
	Logger           *slog.Logger     // default slog.Default()
	DirectoryVersion func() int64     // reported in Welcome; nil → 0
	// SIPAccountFor returns the SIP account a device should register, or
	// nil when it has none. nil func → never included.
	SIPAccountFor func(deviceID string) *wire.SIPAccount
}

func (c Config) withDefaults() Config {
	if c.Heartbeat <= 0 {
		c.Heartbeat = 25 * time.Second
	}
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 5 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 5 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.DirectoryVersion == nil {
		c.DirectoryVersion = func() int64 { return 0 }
	}
	return c
}

// PresenceEvent is emitted when a device connection of one kind comes or goes.
type PresenceEvent struct {
	DeviceID string
	Kind     wire.ClientKind
	Online   bool
}

// Gateway is safe for concurrent use.
type Gateway struct {
	cfg  Config
	auth Authenticator

	mu       sync.Mutex
	sessions map[string]map[wire.ClientKind]*session // deviceID → kind → session
	pending  map[string]map[string]wire.Wake         // deviceID → callID → wake

	onAck      atomic.Pointer[func(deviceID string, ack wire.WakeAck)]
	onPresence atomic.Pointer[func(PresenceEvent)]

	idPrefix string
	idSeq    atomic.Uint64
}

// New constructs a Gateway.
func New(cfg Config, auth Authenticator) *Gateway {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return &Gateway{
		cfg:      cfg.withDefaults(),
		auth:     auth,
		sessions: map[string]map[wire.ClientKind]*session{},
		pending:  map[string]map[string]wire.Wake{},
		idPrefix: "srv-" + hex.EncodeToString(b[:]) + "-",
	}
}

// OnWakeAck registers the callback invoked when a client acknowledges a wake.
func (g *Gateway) OnWakeAck(fn func(deviceID string, ack wire.WakeAck)) { g.onAck.Store(&fn) }

// OnPresence registers the callback invoked on connection attach/detach.
func (g *Gateway) OnPresence(fn func(PresenceEvent)) { g.onPresence.Store(&fn) }

func (g *Gateway) now() time.Time { return g.cfg.Now() }

func (g *Gateway) nextID() string {
	return fmt.Sprintf("%s%d", g.idPrefix, g.idSeq.Add(1))
}

// Serve accepts connections on l until ctx is cancelled or l fails. The
// listener is expected to already be TLS-wrapped; the gateway is transport
// agnostic so tests can drive it over plain TCP.
func (g *Gateway) Serve(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("gateway: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.HandleConn(ctx, conn)
		}()
	}
}

// Online reports whether the device has at least one live connection.
func (g *Gateway) Online(deviceID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sessions[deviceID]) > 0
}

// Wake delivers w to every live connection of deviceID and remembers it until
// ExpiresAt so a reconnecting client receives it again (PROTOCOL.md §7).
// It returns the number of connections the wake was written to; 0 means the
// device is offline and the caller should fall back to APNS (Phase 4b).
func (g *Gateway) Wake(deviceID string, w wire.Wake) int {
	g.mu.Lock()
	if w.ExpiresAt.After(g.now()) {
		if g.pending[deviceID] == nil {
			g.pending[deviceID] = map[string]wire.Wake{}
		}
		g.pending[deviceID][w.CallID] = w
	}
	targets := g.snapshotLocked(deviceID)
	g.mu.Unlock()

	n := 0
	for _, s := range targets {
		if s.sendWake(w) == nil {
			n++
		}
	}
	return n
}

// CancelWake forgets the pending wake and tells live connections to stop ringing.
func (g *Gateway) CancelWake(deviceID, callID string, reason wire.CancelReason) {
	g.mu.Lock()
	delete(g.pending[deviceID], callID)
	targets := g.snapshotLocked(deviceID)
	g.mu.Unlock()
	for _, s := range targets {
		_ = s.send(wire.TypeWakeCancel, wire.WakeCancel{CallID: callID, Reason: reason})
	}
}

// ForgetWake drops the pending wake without telling anyone: the call it
// announced has been answered or has ended, so a client reconnecting later
// must not be rung for it again. (CancelWake would also end the live call
// in a client that answered it.)
func (g *Gateway) ForgetWake(deviceID, callID string) {
	g.mu.Lock()
	delete(g.pending[deviceID], callID)
	if len(g.pending[deviceID]) == 0 {
		delete(g.pending, deviceID)
	}
	g.mu.Unlock()
}

// NotifyDirectory broadcasts a directory_changed to every live connection.
func (g *Gateway) NotifyDirectory(version int64) {
	g.mu.Lock()
	var all []*session
	for _, kinds := range g.sessions {
		for _, s := range kinds {
			all = append(all, s)
		}
	}
	g.mu.Unlock()
	for _, s := range all {
		_ = s.send(wire.TypeDirectoryChanged, wire.DirectoryChanged{Version: version})
	}
}

func (g *Gateway) snapshotLocked(deviceID string) []*session {
	var out []*session
	for _, s := range g.sessions[deviceID] {
		out = append(out, s)
	}
	return out
}

// attach installs s in the session table, superseding any existing
// connection of the same kind for the device.
func (g *Gateway) attach(s *session) {
	g.mu.Lock()
	kinds := g.sessions[s.deviceID]
	if kinds == nil {
		kinds = map[wire.ClientKind]*session{}
		g.sessions[s.deviceID] = kinds
	}
	old := kinds[s.kind]
	kinds[s.kind] = s
	g.mu.Unlock()

	if old != nil {
		// Asynchronous so a stalled old connection cannot delay the new handshake.
		go old.fail(wire.CodeSuperseded, "replaced by a newer connection")
	}
	g.emitPresence(PresenceEvent{DeviceID: s.deviceID, Kind: s.kind, Online: true})
}

func (g *Gateway) detach(s *session) {
	g.mu.Lock()
	kinds := g.sessions[s.deviceID]
	if kinds[s.kind] != s {
		g.mu.Unlock()
		return // already superseded; the newer session owns presence
	}
	delete(kinds, s.kind)
	if len(kinds) == 0 {
		delete(g.sessions, s.deviceID)
	}
	g.mu.Unlock()
	g.emitPresence(PresenceEvent{DeviceID: s.deviceID, Kind: s.kind, Online: false})
}

func (g *Gateway) emitPresence(ev PresenceEvent) {
	if fn := g.onPresence.Load(); fn != nil {
		(*fn)(ev)
	}
}

// pendingFor returns live pending wakes for a device, pruning expired ones.
func (g *Gateway) pendingFor(deviceID string) []wire.Wake {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	var out []wire.Wake
	for id, w := range g.pending[deviceID] {
		if !w.ExpiresAt.After(now) {
			delete(g.pending[deviceID], id)
			continue
		}
		out = append(out, w)
	}
	if len(g.pending[deviceID]) == 0 {
		delete(g.pending, deviceID)
	}
	return out
}

func (g *Gateway) handleAck(s *session, ack wire.WakeAck) {
	g.mu.Lock()
	_, known := g.pending[s.deviceID][ack.CallID]
	if known && ack.Action != wire.WakeWillAnswer {
		delete(g.pending[s.deviceID], ack.CallID)
	}
	g.mu.Unlock()
	if !known {
		_ = s.send(wire.TypeError, wire.Error{Code: wire.CodeUnknownCall, Message: "no pending wake for call_id", Fatal: false})
		return
	}
	if fn := g.onAck.Load(); fn != nil {
		(*fn)(s.deviceID, ack)
	}
}

// ---- session ----------------------------------------------------------------

type session struct {
	g        *Gateway
	conn     net.Conn
	id       string
	deviceID string
	kind     wire.ClientKind
	wmu      sync.Mutex
	closed   atomic.Bool
	// call_ids already written on this connection: a wake that arrives
	// between attach and the pending-wake replay would otherwise go out
	// twice. Guarded by wmu.
	sentWakes map[string]bool
}

// sendWake writes w unless this connection already carried it.
func (s *session) sendWake(w wire.Wake) error {
	s.wmu.Lock()
	if s.sentWakes == nil {
		s.sentWakes = map[string]bool{}
	}
	if s.sentWakes[w.CallID] {
		s.wmu.Unlock()
		return nil
	}
	s.sentWakes[w.CallID] = true
	s.wmu.Unlock()
	return s.send(wire.TypeWake, w)
}

func (s *session) send(t wire.Type, body any) error {
	e, err := wire.New(t, s.g.nextID(), s.g.now(), body)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = s.conn.SetWriteDeadline(time.Now().Add(s.g.cfg.WriteTimeout))
	return wire.WriteFrame(s.conn, e)
}

// fail sends a fatal error then closes the connection.
func (s *session) fail(code wire.ErrorCode, msg string) {
	_ = s.send(wire.TypeError, wire.Error{Code: code, Message: msg, Fatal: true})
	s.close()
}

func (s *session) close() {
	if s.closed.CompareAndSwap(false, true) {
		_ = s.conn.Close()
	}
}

// HandleConn runs the protocol for one accepted connection and returns when
// it closes. Exported so tests and alternative listeners can drive it.
func (g *Gateway) HandleConn(ctx context.Context, conn net.Conn) {
	s := &session{g: g, conn: conn, id: g.nextID()}
	stop := context.AfterFunc(ctx, s.close)
	defer stop()
	defer s.close()
	log := g.cfg.Logger.With("session", s.id, "remote", conn.RemoteAddr().String())

	// Handshake: exactly one Hello within HandshakeTimeout.
	_ = conn.SetReadDeadline(time.Now().Add(g.cfg.HandshakeTimeout))
	e, err := wire.ReadFrame(conn)
	if err != nil {
		if !isTimeout(err) {
			s.fail(wire.CodeBadFrame, "expected a hello frame")
		}
		return
	}
	if err := e.Validate(); err != nil {
		if errors.Is(err, wire.ErrUnsupportedVersion) {
			s.fail(wire.CodeUnsupportedVersion, err.Error())
		} else {
			s.fail(wire.CodeBadFrame, err.Error())
		}
		return
	}
	if e.Type != wire.TypeHello {
		s.fail(wire.CodeHelloExpected, "first frame must be hello")
		return
	}
	var h wire.Hello
	if err := e.DecodeBody(&h); err != nil || h.DeviceID == "" || h.Token == "" ||
		(h.Client != wire.ClientApp && h.Client != wire.ClientExtension) {
		s.fail(wire.CodeBadFrame, "malformed hello")
		return
	}
	ok, err := g.auth.Authenticate(ctx, h.DeviceID, h.Token)
	if err != nil {
		log.Error("authenticate", "err", err)
	}
	if !ok {
		s.fail(wire.CodeUnauthorized, "unknown device or bad token")
		return
	}
	s.deviceID, s.kind = h.DeviceID, h.Client
	log = log.With("device", s.deviceID, "kind", s.kind)

	g.attach(s)
	defer g.detach(s)

	welcome := wire.Welcome{
		SessionID:        s.id,
		HeartbeatSeconds: int((g.cfg.Heartbeat + time.Second - 1) / time.Second),
		ServerTime:       g.now(),
		DirectoryVersion: g.cfg.DirectoryVersion(),
	}
	if g.cfg.SIPAccountFor != nil {
		welcome.SIP = g.cfg.SIPAccountFor(s.deviceID)
	}
	if err := s.send(wire.TypeWelcome, welcome); err != nil {
		return
	}
	for _, w := range g.pendingFor(s.deviceID) {
		_ = s.sendWake(w)
	}
	log.Info("session up")
	defer log.Info("session down")

	idle := 3 * g.cfg.Heartbeat
	for {
		_ = conn.SetReadDeadline(time.Now().Add(idle))
		e, err := wire.ReadFrame(conn)
		if err != nil {
			switch {
			case isTimeout(err):
				s.fail(wire.CodeIdleTimeout, "no frame within 3×heartbeat")
			case errors.Is(err, wire.ErrFrameTooLarge):
				s.fail(wire.CodeBadFrame, err.Error())
			}
			return
		}
		if err := e.Validate(); err != nil {
			s.fail(wire.CodeBadFrame, err.Error())
			return
		}
		switch e.Type {
		case wire.TypePing:
			_ = s.send(wire.TypePong, nil)
		case wire.TypeWakeAck:
			var ack wire.WakeAck
			if err := e.DecodeBody(&ack); err != nil || ack.CallID == "" {
				s.fail(wire.CodeBadFrame, "malformed wake_ack")
				return
			}
			g.handleAck(s, ack)
		default:
			// Unknown or unexpected-direction types are ignored (PROTOCOL.md §2).
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
