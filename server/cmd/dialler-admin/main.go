// Command dialler-admin is the operator's web UI for a Dialler site (SPEC
// §6 item 9c): a second process that speaks only to dialler-server's admin
// API, behind one operator password. It never touches SIP, media or the
// wake gateway, and can crash, restart or be redeployed with no effect on
// a call.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dialler/server/internal/admin"
	"dialler/server/internal/adminui"
	"dialler/server/internal/qr"
	"dialler/server/internal/tlsutil"
)

// version is stamped by the Makefile (-X main.version=…); "dev" otherwise.
var version = "dev"

func main() {
	var (
		listen       = flag.String("listen", "127.0.0.1:8443", "HTTPS listen address for the UI")
		server       = flag.String("server", "https://127.0.0.1:8081", "dialler-server's admin API")
		tokenFile    = flag.String("admin-token-file", "", "file holding the admin bearer token dialler-server was started with (-admin-token-file there); whitespace trimmed")
		adminToken   = flag.String("admin-token", "", "the token itself, for development only (visible in ps)")
		serverCA     = flag.String("server-ca", "", "CA PEM that dialler-server's certificate is verified against (its self-signed certificate file works)")
		insecure     = flag.Bool("insecure", false, "accept any certificate from dialler-server (the self-signed dev certificate)")
		passwordFile = flag.String("password-file", "", "file holding the operator password; whitespace trimmed (required)")
		certFile     = flag.String("tls-cert", "", "TLS certificate PEM for the UI (empty → self-signed, kept in -data-dir)")
		keyFile      = flag.String("tls-key", "", "TLS private key PEM for the UI")
		dataDir      = flag.String("data-dir", "./data/admin", "where the self-signed UI certificate is kept between runs")
		logJSON      = flag.Bool("log-json", false, "log as JSON")
		showVersion  = flag.Bool("version", false, "print the build version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, nil)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	if err := run(context.Background(), log, options{
		listen: *listen, server: *server, tokenFile: *tokenFile, token: *adminToken,
		serverCA: *serverCA, insecure: *insecure, passwordFile: *passwordFile,
		certFile: *certFile, keyFile: *keyFile, dataDir: *dataDir,
	}); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

type options struct {
	listen, server    string
	tokenFile, token  string
	serverCA          string
	insecure          bool
	passwordFile      string
	certFile, keyFile string
	dataDir           string
}

// readSecret reads a token or password file, trimmed; empty is an error.
func readSecret(flagName, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flagName, err)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s: %s is empty", flagName, path)
	}
	return s, nil
}

func run(ctx context.Context, log *slog.Logger, o options) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	token := o.token
	switch {
	case o.tokenFile != "" && o.token != "":
		return errors.New("give -admin-token or -admin-token-file, not both")
	case o.tokenFile != "":
		t, err := readSecret("-admin-token-file", o.tokenFile)
		if err != nil {
			return err
		}
		token = t
	case token == "":
		return errors.New("-admin-token-file is required (the token dialler-server was started with)")
	}
	if o.passwordFile == "" {
		return errors.New("-password-file is required")
	}
	password, err := readSecret("-password-file", o.passwordFile)
	if err != nil {
		return err
	}

	client, err := adminui.NewClient(o.server, token, o.serverCA, o.insecure, 10*time.Second)
	if err != nil {
		return err
	}
	// Tell a wrong token from a dead server at start (ADMIN-API.md §9),
	// but do not refuse to run: the server may simply not be up yet.
	if err := client.Whoami(ctx); err != nil {
		switch {
		case errors.Is(err, adminui.ErrUnauthorized):
			return fmt.Errorf("dialler-server at %s refused the admin token", o.server)
		case adminui.Unreachable(err):
			log.Warn("dialler-server is not answering yet; the UI will show that until it does", "server", o.server, "err", err)
		default:
			log.Warn("whoami", "err", err)
		}
	}

	ui, err := adminui.New(adminui.Config{
		Client:   client,
		Sessions: adminui.NewSessions(password),
		QR: func(link string) (template.HTML, error) {
			c, err := qr.Encode([]byte(link))
			if err != nil {
				return "", err
			}
			return template.HTML(c.SVG()), nil //nolint:gosec // our own encoder's output, no user text in it
		},
		Logger:  log,
		Version: version,
	})
	if err != nil {
		return err
	}

	host, _, err := net.SplitHostPort(o.listen)
	if err != nil {
		return fmt.Errorf("bad -listen %q: %w", o.listen, err)
	}
	hosts := []string{"localhost", "127.0.0.1"}
	if host != "" && host != "0.0.0.0" && host != "::" {
		hosts = append(hosts, host)
	}
	tlsCfg, selfSigned, err := tlsutil.LoadOrKeep(o.certFile, o.keyFile, hosts, filepath.Join(o.dataDir, "tls"))
	if err != nil {
		return err
	}
	if selfSigned {
		log.Warn("serving the UI on a self-signed certificate kept in -data-dir; the browser will warn once")
	}
	ln, err := tls.Listen("tcp", o.listen, tlsCfg)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: ui}
	admin.ConfigureServer(srv)
	log.Info("dialler-admin starting", "listen", ln.Addr(), "server", o.server, "version", version)

	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		return err
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
