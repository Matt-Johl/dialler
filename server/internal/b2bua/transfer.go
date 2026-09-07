package b2bua

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
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
		select {
		case <-a.sess.Context().Done():
			if b != nil {
				_ = b.sess.Hangup(b.sess.Context())
			}
		case <-bDone:
			_ = a.sess.Hangup(a.sess.Context())
		case <-c.swapped:
			continue
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
	c.mu.Unlock()
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

	var newLeg *callLeg
	switch d.Target {
	case routing.Unknown:
		log.Info("transfer: unknown destination")
		return &sipgo404{}
	case routing.Echo:
		newLeg = nil
	case routing.Trunk, routing.Local:
		dst, trunk, err := c.s.transferDestination(ctx, log, c.callID, d, other)
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

	// Final NOTIFY (200 OK, subscription terminated) then release the
	// referrer. baresip ends its call on the 200 NOTIFY by itself.
	notify := referNotify(remoteTarget(from.sess), 200, "OK")
	if _, err := from.sess.Do(ctx, notify); err != nil {
		log.Info("transfer: final NOTIFY failed", "err", err)
	}
	_ = from.sess.Hangup(from.sess.Context())
	from.sess.Close()
	return nil
}

// transferDestination resolves a routed transfer target to a dial URI,
// waking an unregistered local user like a fresh call would.
func (s *Server) transferDestination(ctx context.Context, log *slog.Logger, callID string, d routing.Decision, other *callLeg) (sip.Uri, bool, error) {
	var dst sip.Uri
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

// Errors carrying a SIP status for diago's failure NOTIFY.
type sipgo404 struct{}
type sipgo488 struct{}

func (*sipgo404) Error() string { return "404 Not Found" }
func (*sipgo488) Error() string { return "488 Not Acceptable Here" }
