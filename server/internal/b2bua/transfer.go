package b2bua

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"dialler/server/internal/routing"
	"dialler/server/internal/wire"
)

// callLeg is one side of a bridged call as the relay sees it.
type callLeg struct {
	name  string
	trunk bool
	sess  diago.DialogSession
	codec media.Codec
	// party is who is on this leg, for wakes and logs (the caller's From
	// on the incoming leg, the dialled user on an outgoing one).
	party wire.Party
}

// bridgedCall owns the two legs of a call and the pumps between them, and
// can replace one leg by another on a transfer (SPEC §4.4 rule 6: REFER is
// handled here, never passed to the far leg).
type bridgedCall struct {
	s      *Server
	log    *slog.Logger
	callID string

	mu     sync.Mutex
	a, b   *callLeg // b == nil while a is on echo (self-relay)
	pumps  []*pump
	cancel context.CancelFunc
	// swapped wakes the wait loop so it watches the new leg's context.
	swapped chan struct{}
	// offload is non-nil while the PBX is completing a transfer for us and
	// closed when that attempt is over; offloaded says it succeeded, i.e.
	// finishOffload released both legs and wait() must not hang up either.
	offload   chan struct{}
	offloaded bool
	// woken lists devices a transfer woke for this call; their pending
	// wakes are forgotten when the call ends (the invite path does the same
	// for the callee it woke).
	woken []string
}

func newBridgedCall(s *Server, log *slog.Logger, callID string, a, b *callLeg) *bridgedCall {
	return &bridgedCall{s: s, log: log, callID: callID, a: a, b: b, swapped: make(chan struct{}, 1)}
}

// start (re)starts the relay pumps between the current legs.
func (c *bridgedCall) start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startLocked()
}

func (c *bridgedCall) startLocked() error {
	if c.cancel != nil {
		c.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.pumps = nil
	if c.b == nil {
		// Echo: the remaining party hears itself.
		p, err := newPump(c.a.sess.Media(), c.a.sess.Media())
		if err != nil {
			return err
		}
		c.pumps = []*pump{p}
		go p.run(ctx, c.log.With("dir", c.a.name+"→"+c.a.name))
		return nil
	}
	ab, err := newPump(c.a.sess.Media(), c.b.sess.Media())
	if err != nil {
		return err
	}
	ba, err := newPump(c.b.sess.Media(), c.a.sess.Media())
	if err != nil {
		return err
	}
	c.pumps = []*pump{ab, ba}
	go ab.run(ctx, c.log.With("dir", c.a.name+"→"+c.b.name))
	go ba.run(ctx, c.log.With("dir", c.b.name+"→"+c.a.name))
	return nil
}

// wait blocks until either leg ends, hangs up the other, and stops the
// pumps. A transfer swaps a leg underneath it.
func (c *bridgedCall) wait() {
	for {
		c.mu.Lock()
		a, b := c.a, c.b
		c.mu.Unlock()
		var bDone <-chan struct{}
		if b != nil {
			bDone = b.sess.Context().Done()
		}
		aEnded := false
		select {
		case <-a.sess.Context().Done():
			aEnded = true
		case <-bDone:
		case <-c.swapped:
			continue
		}
		// A transfer the PBX is completing ends the legs on its own terms:
		// let finishOffload do the orderly release (final NOTIFY to the
		// referrer first) rather than racing it with a plain BYE.
		if c.awaitOffload() {
			break
		}
		if aEnded {
			if b != nil {
				_ = b.sess.Hangup(b.sess.Context())
			}
		} else {
			_ = a.sess.Hangup(a.sess.Context())
		}
		break
	}
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	summary := make([]string, 0, len(c.pumps))
	for _, p := range c.pumps {
		summary = append(summary, p.String())
	}
	woken := c.woken
	c.mu.Unlock()
	if c.s.waker != nil {
		for _, dev := range woken {
			c.s.waker.ForgetWake(dev, c.callID)
		}
	}
	c.log.Info("call ended", "relay", strings.Join(summary, " | "))
}

// onRefer is the OnRefer hook for a leg: the party on `from` asked us to
// connect the other party to referTo instead. diago has already answered
// the REFER (202) and sent NOTIFY 100; it dials nothing itself because we
// return before touching the dialog it prepared (we route the target
// ourselves). On success we send the final NOTIFY and hang the referrer
// up; on error diago sends a failure NOTIFY from the returned error.
func (c *bridgedCall) onRefer(from *callLeg) diago.OnReferDialogFunc {
	return func(rd *diago.DialogClientSession) error {
		return c.transfer(from, rd.InviteRequest.Recipient)
	}
}

// transfer replaces `from` with a new leg to referTo, bridged to the other
// party, then releases `from`.
func (c *bridgedCall) transfer(from *callLeg, referTo sip.Uri) error {
	c.mu.Lock()
	other := c.b
	if from == c.b {
		other = c.a
	}
	c.mu.Unlock()
	if other == nil {
		return errors.New("transfer: nothing to transfer (echo)")
	}
	log := c.log.With("transfer_from", from.name, "refer_to", referTo.String())
	log.Info("transfer: requested")

	target := referTarget(referTo)
	d := c.s.router.Resolve(target)
	ctx, cancel := context.WithTimeout(other.sess.Context(), c.s.cfg.RingTimeout)
	defer cancel()

	if shouldOffload(other, d.Target) {
		// Both parties are on the PBX: let it complete the transfer itself
		// and get this server out of the media path (SPEC §4.4 rule 6).
		err := c.offloadToPBX(ctx, log, from, other, d.User)
		var refused *offloadRefused
		switch {
		case err == nil:
			return nil
		case errors.As(err, &refused):
			log.Info("transfer: PBX declined the REFER; handling it here", "why", refused.why)
		default:
			return err
		}
	}

	var newLeg *callLeg
	switch d.Target {
	case routing.Unknown:
		log.Info("transfer: unknown destination")
		return &sipgo404{}
	case routing.Echo:
		newLeg = nil
	case routing.Trunk, routing.Local:
		dst, trunk, err := c.s.transferDestination(ctx, log, c, d, other)
		if err != nil {
			return err
		}
		out, err := c.s.dg.NewDialog(dst, diago.NewDialogOptions{})
		if err != nil {
			return err
		}
		codecs := []media.Codec{other.codec}
		if trunk {
			codecs = trunkCodecs
		}
		out.SetCodecs(codecs)
		leg := &callLeg{name: "transfer", trunk: trunk, sess: out, party: wire.Party{URI: dst.String()}}
		nat := c.s.legNAT(trunk)
		err = out.Invite(ctx, diago.InviteClientOptions{
			Headers:       []sip.Header{sip.NewHeader(CallIDHeader, c.callID)},
			OnMediaUpdate: func(m *diago.DialogMedia) { m.MediaSession().RTPNAT = nat },
			OnRefer:       c.onRefer(leg),
		})
		if err != nil {
			log.Info("transfer: target did not answer", "err", err)
			_ = out.Hangup(out.Context())
			out.Close()
			return err
		}
		if err := out.Ack(ctx); err != nil {
			out.Close()
			return err
		}
		leg.codec = media.CodecAudioFromSession(out.Media().MediaSession())
		if leg.codec.Name != other.codec.Name || leg.codec.SampleRate != other.codec.SampleRate {
			// The relay copies encoded audio; without renegotiating the
			// remaining leg there is nothing we can do with a mismatch.
			log.Info("transfer: codec mismatch, would need transcoding", "target", leg.codec.Name, "remaining", other.codec.Name)
			_ = out.Hangup(out.Context())
			out.Close()
			return &sipgo488{}
		}
		newLeg = leg
	}

	// Swap legs and restart the pumps before releasing the referrer, so
	// the remaining party never hears a gap longer than the dial.
	c.mu.Lock()
	if from == c.a {
		c.a, c.b = other, newLeg
	} else {
		c.b = newLeg
	}
	err := c.startLocked()
	c.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case c.swapped <- struct{}{}:
	default:
	}
	log.Info("transfer: bridged", "remaining", other.name, "to", target)

	c.releaseReferrer(ctx, log, from)
	return nil
}

// How long the referrer gets to hang up on its own after the final NOTIFY.
const referrerByeGrace = 1500 * time.Millisecond

// releaseReferrer reports success to the party that asked for the transfer
// and lets it end its own call: baresip, like any RFC 3515 client, sends
// BYE on the final NOTIFY, so hanging it up ourselves in the same instant
// makes the two BYEs cross and one is answered 481. Give it a moment, then
// hang up only if it is still there.
func (c *bridgedCall) releaseReferrer(ctx context.Context, log *slog.Logger, from *callLeg) {
	notify := referNotify(remoteTarget(from.sess), 200, "OK")
	if _, err := from.sess.Do(ctx, notify); err != nil {
		log.Info("transfer: final NOTIFY to the referrer failed", "err", err)
	}
	select {
	case <-from.sess.Context().Done(): // it hung up on the NOTIFY, as expected
	case <-time.After(referrerByeGrace):
		_ = from.sess.Hangup(ctx)
	}
	from.sess.Close()
}

// transferDestination resolves a routed transfer target to a dial URI,
// waking an unregistered local user like a fresh call would.
func (s *Server) transferDestination(ctx context.Context, log *slog.Logger, c *bridgedCall, d routing.Decision, other *callLeg) (sip.Uri, bool, error) {
	var dst sip.Uri
	callID := c.callID
	if d.Target == routing.Trunk {
		if s.cfg.Trunk == nil {
			return dst, false, errors.New("no trunk")
		}
		err := sip.ParseUri(s.cfg.Trunk.URI(d.User), &dst)
		return dst, true, err
	}
	ep := d.Endpoint
	if d.Registered && !s.cfg.KeepAdvertisedContact && !s.flowAlive(ep.Contact) {
		s.reg.Unregister(ep.User)
		d.Registered = false
	}
	if !d.Registered {
		from := sip.FromHeader{Address: sip.Uri{}}
		_ = sip.ParseUri(other.party.URI, &from.Address)
		from.DisplayName = other.party.DisplayName
		woken, err := s.wakeAndWaitFrom(ctx, log, nil, callID, &from, ep)
		if err != nil {
			return dst, false, err
		}
		c.mu.Lock()
		c.woken = append(c.woken, ep.DeviceID)
		c.mu.Unlock()
		ep = woken
	}
	err := sip.ParseUri(ep.Contact, &dst)
	return dst, false, err
}

// remoteTarget is the far end's Contact for in-dialog requests: the INVITE's
// Contact when they called us, the 2xx's when we called them.
func remoteTarget(d diago.DialogSession) sip.Uri {
	if rc, ok := d.(interface{ RemoteContact() *sip.ContactHeader }); ok {
		if c := rc.RemoteContact(); c != nil {
			return c.Address
		}
	}
	dlg := d.DialogSIP()
	if dlg.InviteResponse != nil {
		if c := dlg.InviteResponse.Contact(); c != nil {
			return c.Address
		}
	}
	if c := dlg.InviteRequest.Contact(); c != nil {
		return c.Address
	}
	return dlg.InviteRequest.Recipient
}

// referTarget turns a Refer-To URI into what the router understands: the
// user part, with our own host kept so foreign domains stay foreign.
func referTarget(u sip.Uri) string {
	if u.Host == "" {
		return u.User
	}
	return "sip:" + u.User + "@" + u.Host
}

// referNotify builds the NOTIFY that reports a REFER's outcome (RFC 3515):
// a sipfrag body with the status, subscription terminated.
func referNotify(remote sip.Uri, code int, reason string) *sip.Request {
	req := sip.NewRequest(sip.NOTIFY, remote)
	req.AppendHeader(sip.NewHeader("Event", "refer"))
	req.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
	req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
	req.SetBody([]byte(fmt.Sprintf("SIP/2.0 %d %s", code, reason)))
	return req
}

// ---- transfer offload to the PBX --------------------------------------------

// shouldOffload: when the party that stays on the call is on the PBX trunk
// and the transfer target is a PBX destination too, the PBX can complete
// the transfer itself and this server leaves the media path entirely.
// Anything involving an app leg, or the server's own echo, stays here.
func shouldOffload(remaining *callLeg, target routing.Target) bool {
	return remaining != nil && remaining.trunk && target == routing.Trunk
}

// offloadRefused: the PBX did not complete the transfer (REFER rejected, a
// failure NOTIFY, or no answer); the server does the transfer itself.
type offloadRefused struct{ why string }

func (e *offloadRefused) Error() string { return "PBX declined the transfer: " + e.why }

// referOutcome classifies a NOTIFY status about our REFER: not final yet
// (1xx), success (2xx), or a refusal.
func referOutcome(code int) (final, success bool) {
	switch {
	case code < 200:
		return false, false
	case code < 300:
		return true, true
	default:
		return true, false
	}
}

// responseStatus is the SIP status inside a sipgo dialog error, or 0.
func responseStatus(err error) int {
	var byVal sipgo.ErrDialogResponse
	var byPtr *sipgo.ErrDialogResponse
	if errors.As(err, &byVal) && byVal.Res != nil {
		return byVal.Res.StatusCode
	}
	if errors.As(err, &byPtr) && byPtr != nil && byPtr.Res != nil {
		return byPtr.Res.StatusCode
	}
	return 0
}

// How long to wait for the PBX to report the transfer's outcome.
const referOutcomeTimeout = 10 * time.Second

// offloadToPBX re-issues the app's transfer to the PBX: our own REFER on
// the remaining party's trunk leg, Refer-To the target as the PBX knows
// it. The PBX moves its endpoint (the phone) to the target, reports the
// outcome in a NOTIFY and hangs our leg up. On success both of our legs
// are released and the referrer gets its final NOTIFY 200; the audio then
// runs phone ↔ PBX ↔ target with this server gone. Returns offloadRefused
// when the PBX would not do it, so transfer() falls back to dialling.
func (c *bridgedCall) offloadToPBX(ctx context.Context, log *slog.Logger, from, remaining *callLeg, user string) error {
	if c.s.cfg.Trunk == nil {
		return &offloadRefused{"no trunk configured"}
	}
	var referTo sip.Uri
	if err := sip.ParseUri(c.s.cfg.Trunk.URI(user), &referTo); err != nil {
		return err
	}
	outcome := make(chan int, 8)
	onNotify := func(code int) {
		select {
		case outcome <- code:
		default:
		}
	}
	var err error
	switch sess := remaining.sess.(type) {
	case *diago.DialogServerSession: // the PBX called us
		err = sess.ReferOptions(ctx, referTo, diago.ReferServerOptions{OnNotify: onNotify})
	case *diago.DialogClientSession: // we called the PBX
		err = sess.ReferOptions(ctx, referTo, diago.ReferClientOptions{OnNotify: onNotify})
	default:
		return &offloadRefused{"leg cannot send REFER"}
	}
	if err != nil {
		if code := responseStatus(err); code != 0 {
			return &offloadRefused{fmt.Sprintf("REFER rejected with %d", code)}
		}
		return &offloadRefused{"REFER failed: " + err.Error()}
	}
	log.Info("transfer: REFER accepted by the PBX; waiting for its outcome", "refer_to", referTo.String())
	c.beginOffload()

	timeout := time.NewTimer(referOutcomeTimeout)
	defer timeout.Stop()
	for {
		select {
		case code := <-outcome:
			final, ok := referOutcome(code)
			if !final {
				continue
			}
			if !ok {
				c.endOffload(false)
				return &offloadRefused{fmt.Sprintf("PBX reported %d", code)}
			}
			c.finishOffload(log, from, remaining, user)
			return nil
		case <-remaining.sess.Context().Done():
			// The PBX moved the phone and hung our leg up before (or
			// instead of) a final NOTIFY: same outcome.
			c.finishOffload(log, from, remaining, user)
			return nil
		case <-timeout.C:
			c.endOffload(false)
			return &offloadRefused{"no final NOTIFY from the PBX"}
		}
	}
}

func (c *bridgedCall) beginOffload() {
	c.mu.Lock()
	c.offload = make(chan struct{})
	c.mu.Unlock()
}

// endOffload records the attempt's result and releases wait() if it is
// holding for it.
func (c *bridgedCall) endOffload(succeeded bool) {
	c.mu.Lock()
	c.offloaded = succeeded
	ch := c.offload
	c.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// awaitOffload is wait()'s side: if a PBX-side transfer is in progress,
// block until it is decided, then report whether the legs were already
// released by finishOffload.
func (c *bridgedCall) awaitOffload() bool {
	c.mu.Lock()
	ch := c.offload
	c.mu.Unlock()
	if ch == nil {
		return false
	}
	<-ch
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offloaded
}

// finishOffload stops relaying, tells the referrer the transfer succeeded
// (baresip ends its call as "transferred" on that NOTIFY), and releases
// both legs. Uses its own context: the legs' contexts may already be over.
func (c *bridgedCall) finishOffload(log *slog.Logger, from, remaining *callLeg, user string) {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel() // no more relaying: the PBX carries the audio now
	}
	summary := make([]string, 0, len(c.pumps))
	for _, p := range c.pumps {
		summary = append(summary, p.String())
	}
	c.mu.Unlock()
	log.Info("transfer: offloaded to PBX", "remaining", remaining.name, "to", user, "relayed_until_now", strings.Join(summary, " | "))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.releaseReferrer(ctx, log, from)
	if remaining.sess.Context().Err() == nil {
		_ = remaining.sess.Hangup(ctx)
	}
	remaining.sess.Close()
	c.endOffload(true)
}

// Errors carrying a SIP status for diago's failure NOTIFY.
type sipgo404 struct{}
type sipgo488 struct{}

func (*sipgo404) Error() string { return "404 Not Found" }
func (*sipgo488) Error() string { return "488 Not Acceptable Here" }
