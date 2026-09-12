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
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"dialler/server/internal/pbx"
	"dialler/server/internal/registry"
	"dialler/server/internal/routing"
	"dialler/server/internal/sipauth"
	"dialler/server/internal/wire"
)

// CallIDHeader carries the wake call_id on the callee-leg INVITE so the app
// can match the SIP call to the CallKit entry (PROTOCOL.md §6).
const CallIDHeader = "X-Dialler-Call-ID"

// Waker is the gateway's wake surface (satisfied by *gateway.Gateway).
type Waker interface {
	Wake(deviceID string, w wire.Wake) int
	CancelWake(deviceID, callID string, reason wire.CancelReason)
	// ForgetWake drops a pending wake silently once its call is over.
	ForgetWake(deviceID, callID string)
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
	// TrunkCodecs is what a PBX trunk is offered, in order of preference
	// (default DefaultTrunkCodecs: G.722, PCMU, PCMA). A PBX that must stay
	// narrowband gets "pcmu,pcma" via -trunk-codecs.
	TrunkCodecs []media.Codec
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
	// Auth challenges every app-leg REGISTER and initial INVITE with SIP
	// Digest against the device enrolment store and refuses a SIP user
	// other than the device's enrolled one (SPEC §4.4 rule 2). nil = no
	// authentication (unit tests only).
	Auth *sipauth.Authenticator
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

	// Calls waiting for a woken app to register, by call id, so a decline or
	// busy wake_ack can end the wait at once instead of at the ring timeout.
	waitMu  sync.Mutex
	waiting map[string]context.CancelCauseFunc
}

// Why a woken callee's wait ended early: the device answered the wake with
// a refusal. The caller is told 486 (see wakeAndWaitFrom).
var (
	errWakeDeclined = errors.New("callee declined")
	errWakeBusy     = errors.New("callee busy")
)

// HandleWakeAck is the gateway's wake_ack hook: a decline or busy from the
// woken device ends that call's wait immediately. will_answer is not an
// event here; the registration that follows it is what the wait is for.
func (s *Server) HandleWakeAck(_ string, ack wire.WakeAck) {
	var cause error
	switch ack.Action {
	case wire.WakeDecline:
		cause = errWakeDeclined
	case wire.WakeBusy:
		cause = errWakeBusy
	default:
		return
	}
	s.waitMu.Lock()
	cancel := s.waiting[ack.CallID]
	s.waitMu.Unlock()
	if cancel != nil {
		cancel(cause)
	}
}

// trackWait makes a call's wake-wait cancellable by HandleWakeAck; the
// returned func removes it and must be deferred by the waiter.
func (s *Server) trackWait(callID string, cancel context.CancelCauseFunc) func() {
	s.waitMu.Lock()
	if s.waiting == nil {
		s.waiting = map[string]context.CancelCauseFunc{}
	}
	s.waiting[callID] = cancel
	s.waitMu.Unlock()
	return func() {
		s.waitMu.Lock()
		delete(s.waiting, callID)
		s.waitMu.Unlock()
		cancel(nil)
	}
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
		diago.WithServerRequestMiddleware(s.authMiddleware),
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
			// diago fills ExternalPort from BindPort only when ExternalHost is
			// empty (diago.go). We always set ExternalHost, so set the port too
			// or the Contact in our responses carries port 0 and the PBX sends
			// its ACK for our 200 OK to the wrong port: over UDP (no reused
			// connection like TLS) the ACK never arrives and answering the
			// inbound leg blocks for 32 s (Timer H) then fails — silent call.
			ExternalPort: port,
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
			// The app leg's offer: Opus for app↔app, G.722 for wideband PBX
			// calls, G.711 as the fallback everything speaks.
			Codecs: []media.Codec{media.CodecAudioOpus, media.CodecAudioG722, media.CodecAudioUlaw, media.CodecAudioAlaw},
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
	if s.cfg.Auth != nil {
		enrolled, err := s.cfg.Auth.Verify(req)
		if !s.authorized(req, tx, enrolled, user, err) {
			return
		}
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

// authorized applies the app-leg Digest verdict for a request whose SIP
// user is sipUser: a request with no or stale credentials is challenged
// (401), bad credentials or an unknown device are refused (403), and so is
// a SIP user other than the one the authenticated device is enrolled for —
// the check that stops one enrolled phone registering or calling as
// another. Returns true when the request may proceed.
func (s *Server) authorized(req *sip.Request, tx sip.ServerTransaction, enrolledUser, sipUser string, err error) bool {
	switch {
	case err == nil:
		if !strings.EqualFold(enrolledUser, sipUser) {
			s.log.Warn("app leg: device is not enrolled for this user", "method", req.Method, "enrolled_user", enrolledUser, "sip_user", sipUser, "src", req.Source())
			_ = tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil))
			return false
		}
		return true
	case errors.Is(err, sipauth.ErrNoCredentials), errors.Is(err, sipauth.ErrStale):
		_ = tx.Respond(s.cfg.Auth.Challenge(req, errors.Is(err, sipauth.ErrStale)))
		return false
	default:
		s.log.Warn("app leg: authentication refused", "method", req.Method, "sip_user", sipUser, "src", req.Source(), "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil))
		return false
	}
}

// authMiddleware gates initial INVITEs on the app leg with the same Digest
// check as REGISTER, requiring the From user to be the device's enrolled
// user (no caller-identity spoofing). In-dialog requests ride a dialog that
// was authenticated when it was set up, and trunk requests are trusted by
// address (SPEC §4.4 rule 7), so neither is challenged.
func (s *Server) authMiddleware(next sipgo.RequestHandler) sipgo.RequestHandler {
	return func(req *sip.Request, tx sip.ServerTransaction) {
		if s.cfg.Auth != nil && req.Method == sip.INVITE && !inDialog(req) &&
			!isTrunkSource(req.Transport(), req.Source(), s.cfg.Trunk) {
			sipUser := ""
			if from := req.From(); from != nil {
				sipUser = from.Address.User
			}
			enrolled, err := s.cfg.Auth.Verify(req)
			if !s.authorized(req, tx, enrolled, sipUser, err) {
				return
			}
		}
		next(req, tx)
	}
}

// inDialog: a To tag means the request belongs to an existing dialog
// (re-INVITE for hold, BYE, ...), not a new call.
func inDialog(req *sip.Request) bool {
	to := req.To()
	return to != nil && to.Params.Has("tag")
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
	} else if wakeable {
		// Answered and over. The wake is kept pending through the ring so a
		// client reconnecting mid-ring is rung again; from here a reconnect
		// must not replay a call that is finished (it rang the app for a
		// dead call, and the app's answer then armed it to take the next
		// INVITE unrung).
		s.waker.ForgetWake(ep.DeviceID, callID)
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

	// The wait can be cut short by the device's own answer to the wake: a
	// decline or busy wake_ack (HandleWakeAck) cancels it with that cause.
	wctx, cancel := context.WithCancelCause(ctx)
	defer s.trackWait(callID, cancel)()

	woken, err := s.reg.WaitRegistered(wctx, ep.User)
	if err != nil {
		cause := context.Cause(wctx)
		if cause == errWakeDeclined || cause == errWakeBusy {
			// The user refused the call. Tell the caller 486 so the PBX
			// applies its busy rule (busy tone / forward-on-busy), not 480,
			// which reads as "nobody there". The device has already stopped
			// ringing, so no wake_cancel is needed.
			log.Info("invite: callee refused the wake", "cause", cause)
			respond(486, "Busy Here")
			return ep, cause
		}
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

// DefaultTrunkCodecs is what a PBX trunk gets offered unless configured:
// G.722 wideband first (what enterprise handsets speak to their PBX), then
// G.711. Apps get the full set (Opus first). Because the relay copies
// encoded audio between the legs, the caller is always answered with the
// codec the callee took, so a call that touches the trunk is G.722 or
// G.711 end to end and never transcoded (SPEC §4.4 rule 4).
var DefaultTrunkCodecs = []media.Codec{media.CodecAudioG722, media.CodecAudioUlaw, media.CodecAudioAlaw}

// trunkCodecs is the configured trunk offer, or the default.
func (c Config) trunkCodecs() []media.Codec {
	if len(c.TrunkCodecs) == 0 {
		return DefaultTrunkCodecs
	}
	return c.TrunkCodecs
}

// ParseCodecs turns a flag value like "g722,pcmu,pcma" into codecs, in that
// order of preference. Names are case-insensitive; PCMU/PCMA/G722/opus.
func ParseCodecs(list string) ([]media.Codec, error) {
	var out []media.Codec
	for _, name := range strings.Split(list, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "":
			continue
		case "g722":
			out = append(out, media.CodecAudioG722)
		case "pcmu", "ulaw", "g711u":
			out = append(out, media.CodecAudioUlaw)
		case "pcma", "alaw", "g711a":
			out = append(out, media.CodecAudioAlaw)
		case "opus":
			out = append(out, media.CodecAudioOpus)
		default:
			return nil, fmt.Errorf("unknown codec %q (want g722, pcmu, pcma or opus)", strings.TrimSpace(name))
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no codecs")
	}
	return out, nil
}

// callTouchesTrunk reports whether either leg is the PBX trunk. When it is,
// the callee is offered G.711 only so the negotiated codec is one both legs
// share (the relay does not transcode); app↔app keeps the full set.
func callTouchesTrunk(l legs) bool { return l.callerTrunk || l.calleeTrunk }

// callerStatus is the final response the caller gets when the callee leg
// fails. A deliberate refusal by the callee — 486 Busy Here (what baresip
// and desk phones send on reject), 600 Busy Everywhere, 603 Decline — is
// relayed unchanged so the PBX applies its busy rule. Anything else (no
// answer, timeout, transport failure, cancelled wait) is 480 Temporarily
// Unavailable: nobody could be reached. diago surfaces the callee's final
// response as sipgo.ErrDialogResponse, by value or by pointer depending on
// the path, so both are checked.
func callerStatus(err error) (int, string) {
	var res *sip.Response
	var byVal sipgo.ErrDialogResponse
	var byPtr *sipgo.ErrDialogResponse
	if errors.As(err, &byVal) {
		res = byVal.Res
	} else if errors.As(err, &byPtr) && byPtr != nil {
		res = byPtr.Res
	}
	if res != nil {
		switch res.StatusCode {
		case 486, 600, 603:
			return int(res.StatusCode), res.Reason
		}
	}
	return 480, "Temporarily Unavailable"
}

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
	// The relay copies encoded audio between the legs without transcoding, so
	// both legs must settle on the same codec. A PBX trunk speaks G.722 and
	// G.711. Constrain the callee's offer to the trunk set whenever EITHER
	// leg is the trunk, not just when the callee is: an app callee reached
	// from a trunk caller, left with its full set, negotiates Opus, which the
	// trunk caller then cannot be answered with — silence in both directions.
	// A call that touches the trunk is therefore G.722 (or G.711) end to end
	// (app↔app, neither trunk, keeps the full set with Opus first). The
	// Originator below intersects this set with the caller's offer, keeping
	// every common codec so the far end can still fall back (vendored patch).
	if callTouchesTrunk(l) {
		out.SetCodecs(s.cfg.trunkCodecs())
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
		log.Error("invite callee", "dst", dst.String(), "offered", codecNames(out.Media().MediaSession()), "err", err)
		_ = out.Hangup(out.Context())
		out.Close()
		if in.Context().Err() == nil {
			// The caller is still ringing, so tell it what happened: a
			// refusal by the callee (486/600/603) is relayed as-is so the
			// PBX applies its busy rule; anything else is 480. A cancelled
			// caller is already answered (487) by the stack.
			code, reason := callerStatus(err)
			_ = in.Respond(code, reason, nil)
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

	// Packet path (nil = byte copy through io.Writer); see forward.
	rr    *media.RTPPacketReader
	rw    *media.RTPPacketWriter
	codec media.Codec // destination: payload type and RTP clock
	// Source timeline → ours (only touched by the relay goroutine).
	haveSrc  bool
	srcSSRC  uint32
	tsOffset uint32
	wroteAny bool
	lastOut  uint32
	t0       time.Time
	ts0      uint32
	// Arrival skew of the source against its own timeline, for the log.
	skewMs     atomic.Int64
	earlyMaxMs atomic.Int64
	lateMaxMs  atomic.Int64
	// Loss, reordering and interarrival jitter as seen from the source's
	// sequence numbers and timing (RFC 3550 §6.4.1), so a lossy leg can be
	// placed without RTCP. Written by the relay goroutine only.
	expectSeq   uint16
	lost        atomic.Int64
	reordered   atomic.Int64
	jitter      float64 // RTP ticks, running estimate
	jitterMs100 atomic.Int64
	lastArrival time.Time
	lastTs      uint32
}

type mediaEnd interface {
	AudioReader(...diago.AudioReaderOption) (io.Reader, error)
	AudioWriter(...diago.AudioWriterOption) (io.Writer, error)
	MediaSession() *media.MediaSession
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
	p := &pump{r: r, w: w}
	// Packet-level forwarding needs the RTP reader and writer themselves
	// (for the headers) and the destination's codec; anything else falls
	// back to the byte copy through io.Writer.
	rr, okR := r.(*media.RTPPacketReader)
	rw, okW := w.(*media.RTPPacketWriter)
	if okR && okW {
		if ms := to.MediaSession(); ms != nil {
			p.setPacketPath(rr, rw, media.CodecAudioFromSession(ms))
		}
	}
	return p, nil
}

func (p *pump) setPacketPath(rr *media.RTPPacketReader, rw *media.RTPPacketWriter, dst media.Codec) {
	p.rr, p.rw, p.codec = rr, rw, dst
}

func (p *pump) String() string {
	return fmt.Sprintf("read=%d written=%d write_errs=%d lost=%d reordered=%d jitter_ms=%.1f early_max_ms=%d late_max_ms=%d",
		p.read.Load(), p.written.Load(), p.writeErrs.Load(), p.lost.Load(), p.reordered.Load(), p.jitterMs(),
		p.earlyMaxMs.Load(), p.lateMaxMs.Load())
}

func (p *pump) jitterMs() float64 { return float64(p.jitterMs100.Load()) / 100 }

// forward sends one payload to the destination leg at once, carrying the
// source packet's own timing.
//
// The library's io.Writer path stamps packets with its own clock and then
// BLOCKS for one packet time (20 ms) per write: it is a player's pacer, and
// a relay built on it can never forward faster than the nominal rate. Any
// packets that queue behind it — the ones that arrived before the relay
// started, or a burst after a network stall — are forwarded at exactly the
// rate new ones arrive, so the queue never drains and every burst becomes
// permanent delay for the rest of the call (the "audio 0–2 s late, varying
// per call" symptom on the app). WriteSamples has no clock; we write
// immediately and force each timestamp to the source's, rebased onto our
// stream, so silence gaps stay gaps and the far end's jitter buffer deals
// with the bursts, as it is designed to.
func (p *pump) forward(payload []byte) error {
	if p.rr == nil {
		_, err := p.w.Write(payload)
		return err
	}
	hdr := p.rr.PacketHeader
	now := time.Now()
	newStream := !p.haveSrc || hdr.SSRC != p.srcSSRC
	if newStream {
		// Rebase this source's timeline onto ours, continuing right after
		// what we last sent so the far end sees one monotonic stream.
		start := p.rw.InitTimestamp()
		if p.haveSrc {
			start = p.lastOut + p.codec.SampleTimestamp()
		}
		p.tsOffset = start - hdr.Timestamp
		p.srcSSRC, p.haveSrc = hdr.SSRC, true
		p.t0, p.ts0 = now, hdr.Timestamp
		p.expectSeq = hdr.SequenceNumber + 1
		p.lastArrival, p.lastTs = now, hdr.Timestamp
	} else {
		// Arrival skew against the source's own timeline since its first
		// packet: negative = early (a burst / backlog draining), positive =
		// late (a stall). Both are what the far end's jitter buffer absorbs.
		elapsed := now.Sub(p.t0)
		expected := time.Duration(hdr.Timestamp-p.ts0) * time.Second / time.Duration(p.codec.SampleRate)
		skew := (elapsed - expected).Milliseconds()
		p.skewMs.Store(skew)
		if -skew > p.earlyMaxMs.Load() {
			p.earlyMaxMs.Store(-skew)
		}
		if skew > p.lateMaxMs.Load() {
			p.lateMaxMs.Store(skew)
		}
		// Sequence accounting (wraparound-safe): a jump forward is loss, a
		// small step back is a reordered packet that was counted lost a
		// moment ago, a large one is the source renumbering its stream.
		switch diff := int16(hdr.SequenceNumber - p.expectSeq); {
		case diff == 0:
			p.expectSeq++
		case diff > 0:
			p.lost.Add(int64(diff))
			p.expectSeq = hdr.SequenceNumber + 1
		case diff > -64:
			p.reordered.Add(1)
			if p.lost.Load() > 0 {
				p.lost.Add(-1)
			}
		default:
			p.expectSeq = hdr.SequenceNumber + 1
		}
		// Interarrival jitter (RFC 3550 §6.4.1): the running mean of how far
		// the arrival spacing strays from the timestamp spacing, in RTP ticks.
		arrival := now.Sub(p.lastArrival).Seconds() * float64(p.codec.SampleRate)
		d := arrival - float64(int32(hdr.Timestamp-p.lastTs))
		if d < 0 {
			d = -d
		}
		p.jitter += (d - p.jitter) / 16
		p.jitterMs100.Store(int64(p.jitter * 100000 / float64(p.codec.SampleRate)))
		p.lastArrival, p.lastTs = now, hdr.Timestamp
	}
	// The writer's next timestamp is where it left off (we never let it
	// advance on its own); move it to where this packet belongs.
	want := hdr.Timestamp + p.tsOffset
	cur := p.rw.InitTimestamp()
	if p.wroteAny {
		cur = p.lastOut
	}
	p.rw.DelayTimestamp(want - cur)
	_, err := p.rw.WriteSamples(payload, 0, hdr.Marker || newStream, p.codec.PayloadType)
	if err == nil {
		p.lastOut, p.wroteAny = want, true
	}
	return err
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
				if werr := p.forward(buf[:n]); werr != nil {
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
				"read_2s", r-lastRead, "written_2s", w-lastWritten,
				"lost", p.lost.Load(), "reordered", p.reordered.Load(), "jitter_ms", p.jitterMs(),
				"skew_ms", p.skewMs.Load(), "early_max_ms", p.earlyMaxMs.Load(), "late_max_ms", p.lateMaxMs.Load())
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

// codecNames lists a session's offered codecs for logs ("-" before any
// media session exists).
func codecNames(ms *media.MediaSession) string {
	if ms == nil {
		return "-"
	}
	names := make([]string, 0, len(ms.Codecs))
	for _, c := range ms.Codecs {
		names = append(names, c.Name)
	}
	return strings.Join(names, ",")
}
