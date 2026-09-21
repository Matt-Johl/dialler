// dialler-admin is the operator's web interface to the call server (SPEC
// §4.8, §6 item 9): a separate process that holds the admin token and a
// login of its own, speaks only to dialler-server's admin API over HTTPS,
// and renders server-side HTML. It never touches SIP, media or the wake
// gateway, so it can be restarted or redeployed with no effect on a call.
//
//	dialler-admin set-password -password-file admin.pw     # once, reads stdin
//	dialler-admin -server https://127.0.0.1:8080 -admin-token … -password-file admin.pw \
//	              -server-ca data/tls/self-signed.pem
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dialler/server/internal/adminui"
	"dialler/server/internal/tlsutil"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "set-password" {
		os.Exit(setPassword(os.Args[2:]))
	}
	var (
		listen       = flag.String("listen", "127.0.0.1:8443", "address to serve the UI on (HTTPS)")
		server       = flag.String("server", "https://127.0.0.1:8080", "the call server's admin API")
		adminToken   = flag.String("admin-token", "", "the call server's -admin-token")
		serverCA     = flag.String("server-ca", "", "PEM the call server's certificate is verified against (its kept self-signed certificate works: data/tls/self-signed.pem); empty → system roots")
		insecure     = flag.Bool("insecure", false, "do not verify the call server's certificate (dev only)")
		passwordFile = flag.String("password-file", "", "file holding the operator's password hash (see set-password)")
		certFile     = flag.String("tls-cert", "", "TLS certificate PEM for the UI (empty → self-signed, kept under -data-dir)")
		keyFile      = flag.String("tls-key", "", "TLS private key PEM for the UI")
		dataDir      = flag.String("data-dir", "./data/admin", "directory for the kept self-signed UI certificate")
		host         = flag.String("host", "", "hostname the UI's self-signed certificate is issued for (default: the listen host)")
		logJSON      = flag.Bool("log-json", false, "log as JSON")
	)
	flag.Parse()

	var log *slog.Logger
	if *logJSON {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if err := run(log, *listen, *server, *adminToken, *serverCA, *insecure, *passwordFile, *certFile, *keyFile, *dataDir, *host); err != nil {
		log.Error("dialler-admin", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, listen, server, adminToken, serverCA string, insecure bool, passwordFile, certFile, keyFile, dataDir, host string) error {
	if adminToken == "" {
		return errors.New("-admin-token is required (the call server's)")
	}
	if passwordFile == "" {
		return errors.New("-password-file is required; create it with: dialler-admin set-password -password-file FILE")
	}
	hash, err := os.ReadFile(passwordFile)
	if err != nil {
		return fmt.Errorf("password file: %w", err)
	}
	base, err := url.Parse(server)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return fmt.Errorf("-server must be an https:// URL, got %q", server)
	}

	// Trust in the call server: its CA (or its own kept self-signed
	// certificate), the system roots, or — dev only, and said so — nothing.
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case insecure:
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // opt-in, warned about
		log.Warn("-insecure: the call server's certificate is not verified")
	case serverCA != "":
		pem, err := os.ReadFile(serverCA)
		if err != nil {
			return fmt.Errorf("-server-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("-server-ca %q holds no certificate", serverCA)
		}
		tlsCfg.RootCAs = pool
	}
	transport := &http.Transport{TLSClientConfig: tlsCfg, ForceAttemptHTTP2: true}

	if host == "" {
		host, _, _ = net.SplitHostPort(listen)
		if host == "" {
			host = "localhost"
		}
	}
	uiTLS, selfSigned, err := tlsutil.LoadOrKeep(certFile, keyFile, []string{host, "localhost", "127.0.0.1"}, filepath.Join(dataDir, "tls"))
	if err != nil {
		return err
	}
	if selfSigned {
		log.Warn("serving the UI with a self-signed certificate, kept under -data-dir; the browser will ask once")
	}

	app, err := adminui.New(adminui.Config{
		Client:       adminui.NewClient(base, adminToken, transport),
		PasswordHash: strings.TrimSpace(string(hash)),
		Secure:       true,
		Logger:       log,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           app,
		TLSConfig:         uiTLS,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("dialler-admin listening", "addr", "https://"+listen, "call_server", server)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// setPassword reads a password from stdin and writes its hash to the file.
func setPassword(args []string) int {
	fs := flag.NewFlagSet("set-password", flag.ContinueOnError)
	file := fs.String("password-file", "", "file to write the hash to")
	if err := fs.Parse(args); err != nil || *file == "" {
		fmt.Fprintln(os.Stderr, "usage: dialler-admin set-password -password-file FILE   (reads the password from stdin)")
		return 2
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(os.Stderr, "no password on stdin")
		return 2
	}
	hash, err := adminui.HashPassword(strings.TrimRight(line, "\r\n"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if err := os.WriteFile(*file, []byte(hash+"\n"), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "password hash written to %s\n", *file)
	return 0
}
