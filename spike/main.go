// Command spike-b2bua is the Phase 0 engine-spike B2BUA: it proves the light
// server can be built on sipgo + diago per SPEC §6. It terminates the app leg
// over TLS, keeps a tiny in-memory registrar (diago handles INVITE but not
// REGISTER), and bridges an inbound call to the callee's registered contact
// with media proxied through the server (the app-leg relay, SPEC §4.4).
//
// This is a spike, not the production server: the registrar is a map, there is
// no wake path, and auth is transport-only. It exists to answer "diago or a
// hand-written B2BUA?" with a running call, not to ship.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"flag"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// registrar is a minimal location table: user part -> contact URI.
type registrar struct {
	mu       sync.RWMutex
	contacts map[string]sip.Uri
	log      *slog.Logger
}

func newRegistrar(log *slog.Logger) *registrar {
	return &registrar{contacts: map[string]sip.Uri{}, log: log}
}

func (r *registrar) handle(req *sip.Request, tx sip.ServerTransaction) {
	to := req.To()
	contact := req.Contact()
	if to == nil || contact == nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
		return
	}
	user := to.Address.User
	expires := 300
	if e := req.GetHeader("Expires"); e != nil {
		if n, err := strconv.Atoi(e.Value()); err == nil {
			expires = n
		}
	}
	if v, ok := contact.Params.Get("expires"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			expires = n
		}
	}

	r.mu.Lock()
	if expires == 0 {
		delete(r.contacts, user)
	} else {
		r.contacts[user] = contact.Address
	}
	r.mu.Unlock()

	r.log.Info("register", "user", user, "contact", contact.Address.String(), "expires", expires)
	res := sip.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(contact)
	res.AppendHeader(sip.NewHeader("Expires", strconv.Itoa(expires)))
	_ = tx.Respond(res)
}

func (r *registrar) lookup(user string) (sip.Uri, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.contacts[user]
	return u, ok
}

func main() {
	var (
		bindHost = flag.String("bind-host", "0.0.0.0", "SIP/media bind host")
		bindPort = flag.Int("bind-port", 5061, "SIP TLS port")
		extHost  = flag.String("ext-host", "", "external host advertised in Contact/SDP (default: bind-host)")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *extHost == "" {
		*extHost = *bindHost
	}

	tlsConf, err := selfSigned(*extHost)
	if err != nil {
		log.Error("tls", "err", err)
		os.Exit(1)
	}

	ua, err := sipgo.NewUA(sipgo.WithUserAgent("dialler-spike"), sipgo.WithUserAgentHostname(*extHost))
	if err != nil {
		log.Error("ua", "err", err)
		os.Exit(1)
	}
	srv, err := sipgo.NewServer(ua, sipgo.WithServerLogger(log))
	if err != nil {
		log.Error("server", "err", err)
		os.Exit(1)
	}

	reg := newRegistrar(log)
	srv.OnRegister(reg.handle)

	dg := diago.NewDiago(ua,
		diago.WithServer(srv),
		diago.WithLogger(log),
		diago.WithTransport(diago.Transport{
			Transport:      "tls",
			BindHost:       *bindHost,
			BindPort:       *bindPort,
			ExternalHost:   *extHost,
			TLSConf:        tlsConf,
			// Advertise sip:...;transport=tls rather than sips: — libre/baresip
			// cannot route to a sips: Contact and closes the call with ENOSYS.
			TLSURINoSIPS:   true,
			RewriteContact: true,
		}),
		diago.WithMediaConfig(diago.MediaConfig{
			Codecs: []media.Codec{media.CodecAudioOpus, media.CodecAudioUlaw, media.CodecAudioAlaw},
		}),
	)

	log.Info("spike-b2bua up", "bind", net.JoinHostPort(*bindHost, strconv.Itoa(*bindPort)), "ext", *extHost)

	err = dg.Serve(ctx, func(in *diago.DialogServerSession) {
		bridgeCall(ctx, log, dg, reg, in)
	})
	if err != nil && ctx.Err() == nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func bridgeCall(ctx context.Context, log *slog.Logger, dg *diago.Diago, reg *registrar, in *diago.DialogServerSession) {
	callee := in.ToUser()
	log.Info("invite", "from", in.FromUser(), "to", callee)

	dst, ok := reg.lookup(callee)
	if !ok {
		log.Warn("callee not registered", "user", callee)
		_ = in.Respond(404, "Not Found", nil)
		return
	}

	step := func(s string) { log.Info("bridge step", "step", s, "to", callee) }

	step("trying")
	_ = in.Trying()
	step("ringing")
	_ = in.Ringing()

	callCtx, cancel := context.WithTimeout(in.Context(), 30*time.Second)
	defer cancel()

	bridge := diago.NewBridge()
	step("answer")
	if err := in.Answer(); err != nil {
		log.Error("answer", "err", err)
		return
	}
	step("add caller to bridge")
	if err := bridge.AddDialogSession(in); err != nil {
		log.Error("bridge add caller", "err", err)
		return
	}

	step("invite callee " + dst.String())
	out, err := dg.InviteBridge(callCtx, dst, &bridge, diago.InviteOptions{})
	if err != nil {
		log.Error("invite callee", "callee", callee, "dst", dst.String(), "err", err)
		_ = in.Hangup(in.Context())
		return
	}
	defer out.Close()

	log.Info("bridged", "from", in.FromUser(), "to", callee)
	defer in.Hangup(in.Context())
	defer out.Hangup(out.Context())

	select {
	case <-in.Context().Done():
	case <-out.Context().Done():
	case <-ctx.Done():
	}
	log.Info("call ended", "to", callee)
}

// selfSigned mirrors the server's dev cert so baresip (verify off) connects.
func selfSigned(host string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "dialler-spike"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}, nil
}
