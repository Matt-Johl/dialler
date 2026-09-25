package b2bua

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// The trunk's pooled TCP/TLS connection can go silently dead — after a
// network interruption on either side the socket stays open here while its
// packets go nowhere, and TCP takes a minute or two to notice. bridge's
// watchdog catches that on the first call to use it (trunkResponseTimeout),
// but that call still pays the wait. Qualifying — the OPTIONS keep-alive
// every PBX runs against its peers — finds it first: an OPTIONS every
// interval over that same connection (sipgo pools by destination, so it is
// the one INVITEs use), and one that goes unanswered for the same timeout
// drops the connection, so the next call dials afresh and rings at once.
// The probe also warms a connection when none is pooled yet.
//
// A silent death can only be found by probing or by trying, so the interval
// is the window in which a call can still hit the dead socket and fall to
// the watchdog; ten seconds keeps that small at negligible cost (six tiny
// requests a minute; Asterisk and CUCM answer OPTIONS without logging).

// trunkQualifier runs the probe loop; probe and onDead are injected so the
// loop is testable without a SIP stack.
type trunkQualifier struct {
	interval time.Duration
	timeout  time.Duration
	// probe sends one OPTIONS and returns nil on any final response.
	probe func(ctx context.Context) error
	// onDead drops the pooled connection.
	onDead func()
	// onState, if set, is told each transition (and the first result), for
	// the status endpoint.
	onState func(up bool, err error)
	log     *slog.Logger
}

func (q *trunkQualifier) state(up bool, err error) {
	if q.onState != nil {
		q.onState(up, err)
	}
}

// run probes every interval until ctx ends. Transitions are logged, not
// every failure: a PBX that is down logs once, and once when it is back.
func (q *trunkQualifier) run(ctx context.Context) {
	t := time.NewTicker(q.interval)
	defer t.Stop()
	dead := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, q.timeout)
		err := q.probe(pctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if !dead {
				q.log.Warn("trunk: not answering OPTIONS; dropping its pooled connection", "err", err, "after", q.timeout)
				q.state(false, err)
			}
			dead = true
			q.onDead()
			continue
		}
		if dead {
			q.log.Info("trunk: answering OPTIONS again")
			q.state(true, nil)
		}
		dead = false
	}
}

// startTrunkQualify starts the qualifier for a TCP/TLS trunk when
// configured (Config.TrunkQualify > 0).
func (s *Server) startTrunkQualify(ctx context.Context) {
	t := s.cfg.Trunk
	if t == nil || s.cfg.TrunkQualify <= 0 || !reliableTransport(t.Transport) {
		return
	}
	client := s.dg.Client(transportTrunk)
	if client == nil {
		s.log.Warn("trunk: no client for the trunk transport; not qualifying")
		return
	}
	target := sip.Uri{Host: t.Host, Port: t.Port, UriParams: sip.NewParams()}
	target.UriParams.Add("transport", strings.ToLower(t.Transport))
	q := &trunkQualifier{
		interval: s.cfg.TrunkQualify,
		timeout:  trunkResponseTimeout,
		log:      s.log,
		probe: func(ctx context.Context) error {
			_, err := client.Do(ctx, sip.NewRequest(sip.OPTIONS, target))
			return err
		},
		onDead: func() { s.dropTrunkConnection(s.log) },
		onState: func(up bool, err error) {
			s.trunk.set(up, err, time.Now())
			if s.cfg.OnTrunkState != nil {
				s.cfg.OnTrunkState(up, err)
			}
		},
	}
	// Up until the probe says otherwise: a trunk is assumed reachable at
	// start, exactly as calls assume it.
	s.trunk.set(true, nil, time.Now())
	s.log.Info("trunk: qualifying with OPTIONS", "every", s.cfg.TrunkQualify, "timeout", trunkResponseTimeout)
	go q.run(ctx)
}
