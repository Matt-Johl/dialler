// Package b2bua is the light server's call controller and app-leg SIP
// element, built on sipgo + diago (Phase 0 engine decision, SPEC §6).
//
// Responsibilities:
//   - terminate the app leg over TLS (SPEC §4.4 rules 1-2);
//   - registrar for provisioned users, feeding the registry (diago handles
//     INVITE dialogs but not REGISTER, so this is ours);
//   - route inbound INVITEs: local + registered → bridge; local + not
//     registered → wake the device via the gateway, wait for it to register,
//     then bridge; unknown → 404; trunk → 503 until the PBX leg (Phase 2);
//   - bridge the two legs with media proxied through the server (rule 3).
package b2bua

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"dialler/server/internal/pbx"
	"dialler/server/internal/registry"
	"dialler/server/internal/routing"
	"dialler/server/internal/wire"
)

// CallIDHeader carries the wake call_id on the callee-leg INVITE so the app
// can match the SIP call to the CallKit entry (PROTOCOL.md §6).
const CallIDHeader = "X-Dialler-Call-ID"

// Waker is the gateway's wake surface (satisfied by *gateway.Gateway).
type Waker interface {
	Wake(deviceID string, w wire.Wake) int
	CancelWake(deviceID, callID string, reason wire.CancelReason)
}

// Config configures the B2BUA.
type Config struct {
	BindHost     string      // default 0.0.0.0
	Port         int         // default 5061
	ExternalHost string      // advertised in Contact/SDP and in wakes; required
	TLS          *tls.Config // required: the app leg is TLS-only
	Domains      []string    // SIP domains we serve; REGISTER for others → 403
	// RTP port range for the media relay (both legs). A fixed range lets a
	// deployment publish/firewall exactly these UDP ports. 0 → ephemeral.
	RTPPortStart int
	RTPPortEnd   int
	// KeepAdvertisedContact disables the route rewrite below (registrations
	// are reached by dialling their advertised Contact). Only for proving
	// in the harness that the rewrite is what makes NAT'd phones reachable.
	KeepAdvertisedContact bool
	// NoSymmetricRTP stops the relay from re-targeting the app leg's media
	// at the source address of the first RTP packet it receives from the
	// phone (symmetric RTP / "comedia"). Needed on NAT'd phones (Phase 4b)
	// and harmless on a LAN, so the default (false) learns. Set it where
	// the phone's observed source is NOT a deliverable reply address —
	// Docker Desktop's UDP port proxy in the harness — or the phone goes
	// silent after its first packet.
	NoSymmetricRTP bool
	// PublicPort is the SIP port apps are told to reach us on (wakes) when
	// it differs from Port (container published on another host port).
	// 0 = Port.
	PublicPort int
	// Trunk is the PBX peer for non-local destinations (SPEC §4.4 rule 7);
	// nil = standalone (app↔app only). Trunk-originated calls arrive on a
	// second listener, TrunkBind ("0.0.0.0:5060"), over Trunk.Transport.
	Trunk     *pbx.Trunk
	TrunkBind string
	// TrunkExternalHost is the address the PBX reaches us at (trunk-leg
	// signalling Contact and media). Empty = this host's first address.
	TrunkExternalHost string
	RingTimeout       time.Duration
	MinExpires        int
	MaxExpires        int
	Logger            *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.BindHost == "" {
		c.BindHost = "0.0.0.0"
	}
	if c.Port == 0 {
		c.Port = 5061
	}
	if c.RingTimeout <= 0 {
		c.RingTimeout = 30 * time.Second
	}
	if c.MinExpires == 0 {
		c.MinExpires = 60
	}
	if c.MaxExpires == 0 {
		c.MaxExpires = 3600
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Server is the app-leg SIP element.
type Server struct {
	cfg    Config
	reg    *registry.Registry
	router *routing.Router
	waker  Waker
	log    *slog.Logger
	dg     *diago.Diago
	tl     *sip.TransportLayer
}

// New builds the server. waker may be nil (no wake path; unregistered users
// get 480 immediately).
func New(cfg Config, reg *registry.Registry, router *routing.Router, waker Waker) (*Server, error) {
	cfg = cfg.withDefaults()
	if cfg.TLS == nil {
		return nil, errors.New("b2bua: TLS config is required (app leg is TLS-only)")
	}
	if cfg.ExternalHost == "" {
		return nil, errors.New("b2bua: ExternalHost is required")
	}
	s := &Server{cfg: cfg, reg: reg, router: router, waker: waker, log: cfg.Logger}

	if cfg.RTPPortStart > 0 && cfg.RTPPortEnd > cfg.RTPPortStart {
		media.RTPPortStart = cfg.RTPPortStart
		media.RTPPortEnd = cfg.RTPPortEnd
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("dialler"), sipgo.WithUserAgentHostname(cfg.ExternalHost))
	if err != nil {
		return nil, err
	}
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(cfg.Logger))
	if err != nil {
		return nil, err
	}
	srv.OnRegister(s.onRegister)
	s.tl = srv.TransportLayer()

	opts := []diago.DiagoOption{
		diago.WithServer(srv),
		diago.WithLogger(cfg.Logger),
	}
	if cfg.Trunk != nil {
		host, port, err := trunkBind(cfg.TrunkBind, cfg.Trunk)
		if err != nil {
			return nil, err
		}
		ext := cfg.TrunkExternalHost
		if ext == "" {
			ext = firstIPv4()
		}
		opts = append(opts, diago.WithTransport(diago.Transport{
			ID:           "trunk",
			Transport:    cfg.Trunk.Transport,
			BindHost:     host,
			BindPort:     port,
			ExternalHost: ext,
		}))
	}
	opts = append(opts, mediaOptions(cfg)...)
	s.dg = diago.NewDiago(ua, opts...)
	return s, nil
}

// mediaOptions is the app-leg transport (always present) and the codec set.
func mediaOptions(cfg Config) []diago.DiagoOption {
	return []diago.DiagoOption{
		diago.WithTransport(diago.Transport{
			Transport:    "tls",
			BindHost:     cfg.BindHost,
			BindPort:     cfg.Port,
			ExternalHost: cfg.ExternalHost,
			TLSConf:      cfg.TLS,
			// libre/baresip cannot route to a sips: Contact (closes with ENOSYS).
			TLSURINoSIPS:   true,
			RewriteContact: true,
		}),
		diago.WithMediaConfig(diago.MediaConfig{
			Codecs: []media.Codec{media.CodecAudioOpus, media.CodecAudioUlaw, media.CodecAudioAlaw},
		}),
	}
}

// Serve listens on the app leg until ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	trunk := "none"
	if s.cfg.Trunk != nil {
		trunk = s.cfg.Trunk.URI("*")
	}
	s.log.Info("b2bua listening", "bind", fmt.Sprintf("%s:%d", s.cfg.BindHost, s.cfg.Port), "external", s.cfg.ExternalHost, "domains", s.cfg.Domains,
		"app_leg_symmetric_rtp", !s.cfg.NoSymmetricRTP, "rewrite_contact", !s.cfg.KeepAdvertisedContact, "trunk", trunk)
	err := s.dg.Serve(ctx, s.serveDialog)
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// ---- registrar --------------------------------------------------------------

func (s *Server) servesDomain(host string) bool {
	if len(s.cfg.Domains) == 0 || host == "" {
		return true
	}
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	for _, d := range s.cfg.Domains {
		if strings.EqualFold(d, host) {
			return true
		}
	}
	return strings.EqualFold(host, s.cfg.ExternalHost)
}

func (s *Server) onRegister(req *sip.Request, tx sip.ServerTransaction) {
	to := req.To()
	if to == nil || to.Address.User == "" {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
		return
	}
	user := to.Address.User
	if !s.servesDomain(to.Address.Host) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil))
		return
	}

	contact := req.Contact()
	if contact == nil {
		// Query: report the current binding.
		ep, ok := s.reg.Lookup(user)
		if !ok {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 404, "Not Found", nil))
			return
		}
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		if ep.Contact != "" {
			res.AppendHeader(sip.NewHeader("Contact", "<"+ep.Contact+">"))
		}
		_ = tx.Respond(res)
		return
	}

	expires := registerExpires(req, contact, s.cfg.MaxExpires)
	if contact.Address.Wildcard || expires == 0 {
		s.reg.Unregister(user)
		s.log.Info("sip unregister", "user", user)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
		return
	}
	if expires < s.cfg.MinExpires {
		res := sip.NewResponseFromRequest(req, 423, "Interval Too Brief", nil)
		res.AppendHeader(sip.NewHeader("Min-Expires", strconv.Itoa(s.cfg.MinExpires)))
		_ = tx.Respond(res)
		return
	}
	if expires > s.cfg.MaxExpires {
		expires = s.cfg.MaxExpires
	}

	// Rewrite the binding to the connection the REGISTER arrived on
	// (RFC 5626 outbound semantics): phones sit behind NAT/firewalls and
	// their advertised Contact is unreachable, but the TLS connection they
	// opened to us is. sipgo pools accepted connections by remote address,
	// so an INVITE addressed to src reuses that connection instead of
	// dialling back.
	uri := contact.Address.String()
	stored := uri
	if !s.cfg.KeepAdvertisedContact {
		stored = rewriteContact(contact.Address, req.Source())
	}
	ep, err := s.reg.Register(user, stored, time.Duration(expires)*time.Second)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 404, "Not Found", nil))
		return
	}
	s.log.Info("sip register", "user", user, "contact", uri, "route", ep.Contact, "expires", expires)
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sip.NewHeader("Contact", fmt.Sprintf("<%s>;expires=%d", uri, expires)))
	res.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(expires)))
	_ = tx.Respond(res)
}

// flowAlive reports whether the TLS connection a rewritten registration
// route points at is still in sipgo's pool. It never dials.
func (s *Server) flowAlive(route string) bool {
	var u sip.Uri
	if err := sip.ParseUri(route, &u); err != nil || u.Host == "" || u.Port == 0 {
		return false
	}
	_, err := s.tl.GetConnection("tls", net.JoinHostPort(u.Host, strconv.Itoa(u.Port)))
	return err == nil
}

// rewriteContact returns the URI to route to for a registration: the
// Contact's user part at the request's source ip:port over TLS. If the
// source cannot be parsed the Contact is kept as advertised.
func rewriteContact(advertised sip.Uri, source string) string {
	host, port, err := net.SplitHostPort(source)
	if err != nil || host == "" {
		return advertised.String()
	}
	u := sip.Uri{User: advertised.User, Host: host}
	if p, err := strconv.Atoi(port); err == nil {
		u.Port = p
	}
	u.UriParams = sip.NewParams()
	u.UriParams.Add("transport", "tls")
	return u.String()
}

// registerExpires returns the requested binding lifetime: the Contact
// "expires" param wins, then the Expires header, then def.
func registerExpires(req *sip.Request, contact *sip.ContactHeader, def int) int {
	if v, ok := contact.Params.Get("expires"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	if h := req.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

// ---- calls ------------------------------------------------------------------

func (s *Server) serveDialog(in *diago.DialogServerSession) {
	callee := in.ToUser()
	callID := in.InviteRequest.CallID().Value()
	log := s.log.With("call", callID, "from", in.FromUser(), "to", callee)

	d := s.router.Resolve(callee)
	if d.Target == routing.Unknown {
		log.Info("invite: unknown destination")
		_ = in.Respond(404, "Not Found", nil)
		return
	}
	legs := legs{callerTrunk: s.isTrunkLeg(in)}

	if d.Target == routing.Echo {
		s.echo(log, in, legs.callerTrunk)
		return
	}

	_ = in.Trying()
	_ = in.Ringing()

	ctx, cancel := context.WithTimeout(in.Context(), s.cfg.RingTimeout)
	defer cancel()

	if d.Target == routing.Trunk {
		// Non-local destination: hand it to the PBX as a trunk peer. The
		// PBX does the ringing; we relay media between the two legs.
		if s.cfg.Trunk == nil {
			_ = in.Respond(503, "Service Unavailable", nil)
			return
		}
		var dst sip.Uri
		if err := sip.ParseUri(s.cfg.Trunk.URI(d.User), &dst); err != nil {
			log.Error("bad trunk uri", "err", err)
			_ = in.Respond(500, "Server Internal Error", nil)
			return
		}
		legs.calleeTrunk = true
		log.Info("invite: to trunk", "dst", dst.String(), "from_trunk", legs.callerTrunk)
		_ = s.bridge(ctx, log, in, dst, callID, legs)
		return
	}

	ep := d.Endpoint
	if d.Registered && !s.cfg.KeepAdvertisedContact && !s.flowAlive(ep.Contact) {
		// The registration was bound to a connection that no longer exists
		// (phone backgrounded, network changed, app killed). Dialling its
		// NAT'd address can only time out; treat it as unregistered so the
		// wake path brings the phone back on a fresh connection.
		log.Info("invite: registration flow gone, treating as unregistered", "route", ep.Contact)
		s.reg.Unregister(ep.User)
		d.Registered = false
	}
	wakeable := ep.DeviceID != "" && s.waker != nil
	if !d.Registered {
		woken, err := s.wakeAndWait(ctx, log, in, callID, ep)
		if err != nil {
			return
		}
		ep = woken
	} else if wakeable {
		// Registered: the INVITE goes straight to the app's SIP stack, but
		// send the wake too — the app rings from whichever arrives first and
		// de-duplicates on call_id (PROTOCOL.md §6).
		s.waker.Wake(ep.DeviceID, s.wakeFor(callID, in, ep))
	}

	var dst sip.Uri
	if err := sip.ParseUri(ep.Contact, &dst); err != nil {
		log.Error("bad registered contact", "contact", ep.Contact, "err", err)
		_ = in.Respond(500, "Server Internal Error", nil)
		return
	}
	if err := s.bridge(ctx, log, in, dst, callID, legs); err != nil && wakeable {
		// Never bridged: stop the app ringing.
		reason := wire.CancelTimeout
		if in.Context().Err() != nil {
			reason = wire.CancelCallerHangup
		}
		s.waker.CancelWake(ep.DeviceID, callID, reason)
	}
}

// wakeFor builds the wake payload for an inbound call to ep.
func (s *Server) wakeFor(callID string, in *diago.DialogServerSession, ep registry.Endpoint) wire.Wake {
	return s.wakeForFrom(callID, in.InviteRequest.From(), ep)
}

func (s *Server) wakeForFrom(callID string, from *sip.FromHeader, ep registry.Endpoint) wire.Wake {
	party := wire.Party{}
	if from != nil {
		party = wire.Party{DisplayName: from.DisplayName, URI: from.Address.String()}
	}
	return wire.Wake{
		CallID:    callID,
		From:      party,
		To:        wire.Party{URI: fmt.Sprintf("sip:%s@%s", ep.User, s.primaryDomain())},
		SIP:       wire.SIPTarget{Host: s.cfg.ExternalHost, Port: s.publicPort(), Transport: "tls"},
		ExpiresAt: time.Now().Add(s.cfg.RingTimeout),
	}
}

// wakeAndWait sends a wake for the callee's device and blocks until it
// registers, the caller gives up, or the ring timeout passes. On failure it
// has already answered the caller and cancelled the wake.
func (s *Server) wakeAndWait(ctx context.Context, log *slog.Logger, in *diago.DialogServerSession, callID string, ep registry.Endpoint) (registry.Endpoint, error) {
	return s.wakeAndWaitFrom(ctx, log, in, callID, in.InviteRequest.From(), ep)
}

// wakeAndWaitFrom is wakeAndWait with the caller identity given explicitly
// (a transfer wakes the target on behalf of the remaining party). With a
// nil `in` no SIP response is sent; the caller reports the failure.
func (s *Server) wakeAndWaitFrom(ctx context.Context, log *slog.Logger, in *diago.DialogServerSession, callID string, from *sip.FromHeader, ep registry.Endpoint) (registry.Endpoint, error) {
	respond := func(code int, reason string) {
		if in != nil {
			_ = in.Respond(code, reason, nil)
		}
	}
	if ep.DeviceID == "" || s.waker == nil {
		log.Info("invite: callee not registered and not wake-routable")
		respond(480, "Temporarily Unavailable")
		return ep, errors.New("no wake path")
	}

	delivered := s.waker.Wake(ep.DeviceID, s.wakeForFrom(callID, from, ep))
	if delivered == 0 {
		// Nobody to wake (no live connection; APNS is Phase 4b). Fail fast
		// rather than making the caller wait out the ring timeout.
		s.waker.CancelWake(ep.DeviceID, callID, wire.CancelTimeout)
		log.Info("invite: callee offline, wake undeliverable")
		respond(480, "Temporarily Unavailable")
		return ep, errors.New("wake undeliverable")
	}
	log.Info("invite: woke callee device", "device", ep.DeviceID, "connections", delivered)

	woken, err := s.reg.WaitRegistered(ctx, ep.User)
	if err != nil {
		reason := wire.CancelTimeout
		if ctx.Err() != nil && (in == nil || in.Context().Err() != nil) {
			reason = wire.CancelCallerHangup
		}
		s.waker.CancelWake(ep.DeviceID, callID, reason)
		log.Info("invite: wake did not produce a registration", "reason", reason)
		if reason == wire.CancelTimeout {
			respond(480, "Temporarily Unavailable")
		}
		return ep, err
	}
	return woken, nil
}

// bridge answers the caller and dials the callee's contact, proxying media.
//
// Both legs run symmetric RTP (SPEC §4.4 rule 3, and a must for phones
// behind NAT): the relay learns each peer's real media address from the
// first packet it receives instead of trusting the SDP, whose address is
// often a private one.
// echo answers the caller and plays its own audio straight back: the
// end-to-end self-test for a phone (microphone → encoder → RTP → relay →
// RTP → decoder → speaker) with no second party. Same relay pump as a
// bridged call, source and sink on one leg.
func (s *Server) echo(log *slog.Logger, in *diago.DialogServerSession, trunk bool) {
	_ = in.Trying()
	if err := in.AnswerOptions(diago.AnswerOptions{RTPNAT: s.legNAT(trunk)}); err != nil {
		log.Error("echo: answer", "err", err)
		return
	}
	p, err := newPump(in, in)
	if err != nil {
		log.Error("echo: relay", "err", err)
		_ = in.Hangup(in.Context())
		return
	}
	codec := media.CodecAudioFromSession(in.Media().MediaSession())
	log.Info("echo: answered", "codec", codec.Name, "from_trunk", trunk)
	go p.run(in.Context(), log.With("dir", "echo"))
	<-in.Context().Done()
	log.Info("echo: ended", "relayed", p.String())
}

// legs says which side of a call is the PBX trunk (neither, for app↔app).
type legs struct {
	callerTrunk bool
	calleeTrunk bool
}

// trunkCodecs is what a PBX trunk gets offered: G.711 only. Apps get the
// full set (Opus first). Because the relay copies encoded audio between
// the legs, the caller is always answered with the codec the callee took.
var trunkCodecs = []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}

// legNAT is the symmetric-RTP setting for a leg: the trunk always learns
// (a PBX behind NAT is normal); app legs follow the deployment flag.
func (s *Server) legNAT(trunk bool) int {
	if trunk || !s.cfg.NoSymmetricRTP {
		return media.RTPNATSymetric
	}
	return media.RTPNATDisabled
}

// isTrunkLeg reports whether an incoming call came from the PBX trunk
// rather than an app: the app leg is TLS-only, so any other transport is
// the trunk listener; a TLS trunk is told apart by source address.
func (s *Server) isTrunkLeg(in *diago.DialogServerSession) bool {
	if s.cfg.Trunk == nil {
		return false
	}
	return isTrunkSource(in.InviteRequest.Transport(), in.InviteRequest.Source(), s.cfg.Trunk)
}

func isTrunkSource(transport, source string, t *pbx.Trunk) bool {
	if t == nil {
		return false
	}
	if !strings.EqualFold(transport, "tls") {
		return true
	}
	host, _, err := net.SplitHostPort(source)
	if err != nil {
		host = source
	}
	return strings.EqualFold(host, t.Host)
}

// trunkBind parses the trunk listener address; an empty host is 0.0.0.0 and
// an empty address is port 5060 (5061 for a TLS trunk).
func trunkBind(addr string, t *pbx.Trunk) (string, int, error) {
	if addr == "" {
		port := 5060
		if t.Transport == "tls" {
			port = 5061
		}
		return "0.0.0.0", port, nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("b2bua: bad trunk bind %q: %w", addr, err)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("b2bua: bad trunk bind port %q", portStr)
	}
	return host, port, nil
}

// firstIPv4 is this host's first non-loopback IPv4 address, the address a
// PBX on the same network reaches us at.
func firstIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return "127.0.0.1"
}

func (s *Server) bridge(ctx context.Context, log *slog.Logger, in *diago.DialogServerSession, dst sip.Uri, callID string, l legs) error {
	// Order matters: the caller keeps ringing (180 already sent) until the
	// phone answers, and is answered (200) only then. Answering the caller
	// first — as diago's own bridge helper does — starts the caller's media
	// while the phone is still ringing; a few seconds of it are gone before
	// anyone hears it, and a human caller is "connected" to silence.
	//
	// The callee leg is offered the caller's codecs (Originator reads the
	// caller's INVITE SDP, which does not require it to be answered) and
	// symmetric RTP where the deployment wants it.
	out, err := s.dg.NewDialog(dst, diago.NewDialogOptions{})
	if err != nil {
		log.Error("new callee dialog", "dst", dst.String(), "err", err)
		_ = in.Respond(500, "Server Internal Error", nil)
		return err
	}
	calleeNAT := s.legNAT(l.calleeTrunk)
	if l.calleeTrunk {
		out.SetCodecs(trunkCodecs)
	}
	from := in.InviteRequest.From()
	legA := &callLeg{name: "caller", trunk: l.callerTrunk, sess: in, party: wire.Party{DisplayName: from.DisplayName, URI: from.Address.String()}}
	legB := &callLeg{name: "callee", trunk: l.calleeTrunk, sess: out, party: wire.Party{URI: dst.String()}}
	call := newBridgedCall(s, log, callID, legA, legB)
	err = out.Invite(ctx, diago.InviteClientOptions{
		Originator:    in,
		Headers:       []sip.Header{sip.NewHeader(CallIDHeader, callID)},
		OnMediaUpdate: func(m *diago.DialogMedia) { m.MediaSession().RTPNAT = calleeNAT },
		OnRefer:       call.onRefer(legB),
	})
	if err != nil {
		log.Error("invite callee", "dst", dst.String(), "err", err)
		_ = out.Hangup(out.Context())
		out.Close()
		if in.Context().Err() == nil {
			// Callee did not answer (timeout, decline, failure); the caller
			// is still ringing, so tell it. A cancelled caller is already
			// answered (487) by the stack.
			_ = in.Respond(480, "Temporarily Unavailable", nil)
		}
		return err
	}
	// The callee answered: answer the caller now with the codec the callee
	// took (the relay copies encoded audio, so the legs must match), then
	// complete the callee.
	negotiated := media.CodecAudioFromSession(out.Media().MediaSession())
	log.Info("callee answered", "codec", negotiated.Name, "callee_trunk", l.calleeTrunk, "caller_trunk", l.callerTrunk)
	legA.codec, legB.codec = negotiated, negotiated
	if err := in.AnswerOptions(diago.AnswerOptions{RTPNAT: s.legNAT(l.callerTrunk), Codecs: []media.Codec{negotiated}, OnRefer: call.onRefer(legA)}); err != nil {
		log.Error("answer caller", "err", err)
		_ = out.Hangup(out.Context())
		out.Close()
		return err
	}
	if err := out.Ack(ctx); err != nil {
		log.Error("ack callee", "err", err)
		out.Close()
		_ = in.Hangup(in.Context())
		return err
	}
	defer out.Close()
	log.Info("bridged", "dst", dst.String())

	// Media relay: our own pumps rather than diago's Bridge, so every leg
	// reports what it read and wrote — a phone that receives nothing while
	// "phone<-caller" keeps writing is the network eating packets, not us.
	// The bridged call also handles REFER (transfer) by swapping a leg.
	if err := call.start(); err != nil {
		log.Error("relay", "err", err)
		_ = out.Hangup(out.Context())
		_ = in.Hangup(in.Context())
		return err
	}
	call.wait()
	return nil
}

// pump moves encoded audio one way between two legs and counts it.
type pump struct {
	r          io.Reader
	w          io.Writer
	read       atomic.Int64 // frames read from the source leg
	written    atomic.Int64 // frames written to the destination leg
	writeErrs  atomic.Int64
	lastReadAt atomic.Int64 // unix ms of the last successful read
}

type mediaEnd interface {
	AudioReader(...diago.AudioReaderOption) (io.Reader, error)
	AudioWriter(...diago.AudioWriterOption) (io.Writer, error)
}

func newPump(from, to mediaEnd) (*pump, error) {
	r, err := from.AudioReader()
	if err != nil {
		return nil, err
	}
	w, err := to.AudioWriter()
	if err != nil {
		return nil, err
	}
	return &pump{r: r, w: w}, nil
}

func (p *pump) String() string {
	return fmt.Sprintf("read=%d written=%d write_errs=%d", p.read.Load(), p.written.Load(), p.writeErrs.Load())
}

// run relays until the source leg's reader ends (its session closed) or
// ctx is done, logging a counter line every 2 s so a silent leg can be
// placed: reads flat = nothing arriving from that leg; writes flat with
// reads rising = we stopped forwarding; both rising while the far end
// hears nothing = the packets leave us and are lost on the way.
func (p *pump) run(ctx context.Context, log *slog.Logger) {
	buf := make([]byte, media.RTPBufSize)
	var lastRead, lastWritten int64
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			n, err := p.r.Read(buf)
			if ctx.Err() != nil {
				return // pumps replaced (transfer) or call over
			}
			if n > 0 {
				p.read.Add(1)
				p.lastReadAt.Store(time.Now().UnixMilli())
				if _, werr := p.w.Write(buf[:n]); werr != nil {
					if p.writeErrs.Add(1) <= 3 {
						log.Warn("relay write failed", "err", werr, "after", p.String())
					}
				} else {
					p.written.Add(1)
				}
			}
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					log.Info("relay source closed", "after", p.String())
				} else {
					// Any other error ends the relay for this direction: the
					// far end goes silent. Say so loudly.
					log.Error("RELAY STOPPED on read error", "err", err, "after", p.String())
				}
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			r, w := p.read.Load(), p.written.Load()
			log.Info("relay", "read", r, "written", w, "write_errs", p.writeErrs.Load(),
				"read_2s", r-lastRead, "written_2s", w-lastWritten)
			lastRead, lastWritten = r, w
		}
	}
}

func (s *Server) publicPort() int {
	if s.cfg.PublicPort > 0 {
		return s.cfg.PublicPort
	}
	return s.cfg.Port
}

func (s *Server) primaryDomain() string {
	if len(s.cfg.Domains) > 0 {
		return s.cfg.Domains[0]
	}
	return s.cfg.ExternalHost
}
