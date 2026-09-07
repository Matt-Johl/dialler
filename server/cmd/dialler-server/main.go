// Command dialler-server is the on-prem light server: signal gateway (wire
// protocol over TLS), SIP registrar on the app leg, media relay, directory
// and enrolment APIs. See SPEC.md §4.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"dialler/server/internal/b2bua"
	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/gateway"
	"dialler/server/internal/pbx"
	"dialler/server/internal/registry"
	"dialler/server/internal/routing"
	"dialler/server/internal/tlsutil"
	"dialler/server/internal/wire"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
)

func main() {
	var (
		signalAddr  = flag.String("signal-addr", ":7443", "wire-protocol TLS listen address")
		sipAddr     = flag.String("sip-addr", ":5061", "app-leg SIP/TLS listen address")
		httpAddr    = flag.String("http-addr", "127.0.0.1:8080", "directory + admin HTTP listen address")
		certFile    = flag.String("tls-cert", "", "TLS certificate PEM (empty → self-signed dev cert)")
		keyFile     = flag.String("tls-key", "", "TLS private key PEM")
		publicHost  = flag.String("public-host", "", "hostname/IP advertised to apps in wakes and certs (default: first non-loopback IPv4)")
		localDomain = flag.String("local-domain", "", "SIP domain this server owns (default: public-host)")
		dataDir     = flag.String("data-dir", "./data", "directory for devices.json and directory.json")
		adminToken  = flag.String("admin-token", "", "bearer token for admin APIs (empty → generated and printed)")
		ringTimeout = flag.Duration("ring-timeout", 30*time.Second, "how long a callee may ring (and a woken app may take to register)")
		rtpMin      = flag.Int("rtp-min", 20000, "first UDP port for relayed media (0 = ephemeral)")
		rtpMax      = flag.Int("rtp-max", 20100, "last UDP port for relayed media")
		rewrite     = flag.Bool("rewrite-contact", true, "route to a registration's source address over its own TLS connection (required for phones behind NAT); false only for harness negative tests")
		logJSON     = flag.Bool("log-json", false, "log as JSON")
		sipTrace    = flag.Bool("sip-trace", false, "log every SIP message sent and received (harness debugging)")
		logLevel    = flag.String("log-level", "info", "debug|info|warn|error (debug includes the media relay's RTP source learning)")
		rtpSym      = flag.Bool("rtp-symmetric", true, "re-target a phone's media at the source of its first RTP packet (needed behind NAT); false where that source is not a deliverable reply address (Docker Desktop harness)")
		pubSIPPort  = flag.Int("public-sip-port", 0, "SIP port advertised to apps (welcome, wakes) when it differs from -sip-addr, e.g. a container published on another host port; 0 = same as -sip-addr")
		trunk       = flag.String("trunk", "", "PBX SIP peer for non-local destinations, e.g. sip:asterisk:5060;transport=tcp (empty: standalone, app↔app only)")
		trunkAddr   = flag.String("trunk-addr", "", "listen address for trunk-originated calls (default :5060, :5061 for a TLS trunk); transport follows -trunk")
		trunkExt    = flag.String("trunk-external-host", "", "address the PBX reaches this server at, used in trunk-leg Contact and SDP (default: this host's first address)")
	)
	flag.Parse()
	sip.SIPDebug = *sipTrace

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "bad -log-level:", err)
		os.Exit(2)
	}
	hopts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, hopts)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, hopts)
	}
	log := slog.New(handler)
	slog.SetDefault(log)
	media.SetDefaultLogger(log)

	if err := run(context.Background(), log, options{
		signalAddr: *signalAddr, sipAddr: *sipAddr, httpAddr: *httpAddr,
		certFile: *certFile, keyFile: *keyFile,
		publicHost: *publicHost, localDomain: *localDomain,
		dataDir: *dataDir, adminToken: *adminToken,
		ringTimeout: *ringTimeout, rtpMin: *rtpMin, rtpMax: *rtpMax,
		keepAdvertisedContact: !*rewrite,
		noSymmetricRTP:        !*rtpSym,
		publicSIPPort:         *pubSIPPort,
		trunk:                 *trunk,
		trunkAddr:             *trunkAddr,
		trunkExternalHost:     *trunkExt,
	}); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type options struct {
	signalAddr, sipAddr, httpAddr string
	certFile, keyFile             string
	publicHost, localDomain       string
	dataDir, adminToken           string
	ringTimeout                   time.Duration
	rtpMin, rtpMax                int
	keepAdvertisedContact         bool
	noSymmetricRTP                bool
	publicSIPPort                 int
	trunk, trunkAddr              string
	trunkExternalHost             string
}

func run(ctx context.Context, log *slog.Logger, o options) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if o.publicHost == "" {
		o.publicHost = firstIPv4()
	}
	if o.localDomain == "" {
		o.localDomain = o.publicHost
	}
	if o.adminToken == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		o.adminToken = hex.EncodeToString(b[:])
		log.Warn("no -admin-token given; generated one for this run", "admin_token", o.adminToken)
	}

	tlsCfg, selfSigned, err := tlsutil.Load(o.certFile, o.keyFile, []string{o.publicHost, "localhost", "127.0.0.1"})
	if err != nil {
		return err
	}
	if selfSigned {
		log.Warn("using a self-signed TLS certificate; clients must pin or disable verification in dev")
	}

	// Stores.
	devices, err := enroll.Open(filepath.Join(o.dataDir, "devices.json"))
	if err != nil {
		return err
	}
	dir, err := directory.Open(filepath.Join(o.dataDir, "directory.json"))
	if err != nil {
		return err
	}

	// Core.
	reg := registry.New(nil)
	for _, d := range devices.Devices() {
		if !d.Revoked {
			reg.Provision(d.User, d.DeviceID)
		}
	}
	var adapter pbx.Adapter = pbx.None{}
	var trunkCfg *pbx.Trunk
	if o.trunk != "" {
		t, err := pbx.ParseTrunk(o.trunk)
		if err != nil {
			return fmt.Errorf("bad -trunk %q: %w", o.trunk, err)
		}
		trunkCfg = &t
		adapter = pbx.NewSIPTrunk(t)
	}
	router := routing.New(reg, []string{o.localDomain, o.publicHost}, func() bool { _, ok := adapter.Trunk(); return ok })

	sipHost, sipPortStr, err := net.SplitHostPort(o.sipAddr)
	if err != nil {
		return fmt.Errorf("bad -sip-addr %q: %w", o.sipAddr, err)
	}
	sipPort, err := strconv.Atoi(sipPortStr)
	if err != nil {
		return fmt.Errorf("bad -sip-addr port %q: %w", sipPortStr, err)
	}
	publicSIPPort := o.publicSIPPort
	if publicSIPPort == 0 {
		publicSIPPort = sipPort
	}

	gw := gateway.New(gateway.Config{
		Logger:           log,
		DirectoryVersion: dir.Version,
		// Tell each connecting app which SIP user to register, so it is
		// reachable directly while running (SPEC §2 foreground path).
		SIPAccountFor: func(deviceID string) *wire.SIPAccount {
			ep, ok := reg.LookupDevice(deviceID)
			if !ok {
				return nil
			}
			return &wire.SIPAccount{User: ep.User, Domain: o.localDomain, Host: o.publicHost, Port: publicSIPPort, Transport: "tls"}
		},
	}, devices)
	dir.OnChange(gw.NotifyDirectory)
	gw.OnPresence(func(ev gateway.PresenceEvent) {
		log.Info("presence", "device", ev.DeviceID, "kind", ev.Kind, "online", ev.Online)
	})
	gw.OnWakeAck(func(deviceID string, ack wire.WakeAck) { // replaced by the call controller
		log.Info("wake_ack", "device", deviceID, "call", ack.CallID, "action", ack.Action)
	})

	// App-leg SIP element: registrar + call controller + media bridge (diago).
	calls, err := b2bua.New(b2bua.Config{
		BindHost:              sipHost,
		Port:                  sipPort,
		PublicPort:            publicSIPPort,
		ExternalHost:          o.publicHost,
		TLS:                   tlsCfg,
		Domains:               []string{o.localDomain, o.publicHost},
		RTPPortStart:          o.rtpMin,
		RTPPortEnd:            o.rtpMax,
		KeepAdvertisedContact: o.keepAdvertisedContact,
		NoSymmetricRTP:        o.noSymmetricRTP,
		Trunk:                 trunkCfg,
		TrunkBind:             o.trunkAddr,
		TrunkExternalHost:     o.trunkExternalHost,
		RingTimeout:           o.ringTimeout,
		Logger:                log,
	}, reg, router, gw)
	if err != nil {
		return err
	}

	// HTTP: directory + admin.
	mux := http.NewServeMux()
	mux.Handle("/v1/directory", directory.NewHandler(dir, devices.DeviceAuth, o.adminToken))
	mux.Handle("/v1/directory/", directory.NewHandler(dir, devices.DeviceAuth, o.adminToken))
	mux.Handle("/v1/admin/", enroll.NewAdminHandler(devices, o.adminToken, enroll.Hooks{
		OnIssue: func(deviceID, user string) { reg.Provision(user, deviceID) },
		OnRevoke: func(deviceID string) {
			if ep, ok := reg.LookupDevice(deviceID); ok {
				reg.Deprovision(ep.User)
			}
		},
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })

	// Listeners.
	signalLn, err := tls.Listen("tcp", o.signalAddr, tlsCfg)
	if err != nil {
		return fmt.Errorf("signal listen: %w", err)
	}
	httpSrv := &http.Server{Addr: o.httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	log.Info("dialler-server starting",
		"signal", signalLn.Addr(), "sip", o.sipAddr, "http", o.httpAddr,
		"public_host", o.publicHost, "local_domain", o.localDomain,
		"ring_timeout", o.ringTimeout, "rtp_range", fmt.Sprintf("%d-%d", o.rtpMin, o.rtpMax), "data_dir", o.dataDir)

	errc := make(chan error, 3)
	go func() { errc <- gw.Serve(ctx, signalLn) }()
	go func() { errc <- calls.Serve(ctx) }()
	go func() {
		err := httpSrv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		if err != nil {
			stop()
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return nil
}

func firstIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP.String()
		}
	}
	return "127.0.0.1"
}
