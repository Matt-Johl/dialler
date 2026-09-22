package main

import (
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/emiago/sipgo/sip"

	"dialler/server/internal/b2bua"
	"dialler/server/internal/enroll"
	"dialler/server/internal/pbx"
	"dialler/server/internal/pbxline"
)

// -pbx-mode: how this server presents itself to the PBX (SPEC §6 item 3c).
//
// The two modes are exclusive by design, not by omission. An exchange
// matches inbound SIP against its trunk devices by source address and port,
// so a trunk pointing at this server would swallow the calls our registered
// lines place and the registrations would count for nothing. Choosing one
// per deployment removes that whole class of fault; it also means a trunk
// deployment cannot be affected by any of this, because in trunk mode none
// of it is built.
func parsePBXMode(s string) (lines bool, err error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "trunk":
		return false, nil
	case "lines":
		return true, nil
	default:
		return false, fmt.Errorf("want trunk or lines, got %q", s)
	}
}

// splitList reads a comma-separated flag, dropping blanks.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// registrarURI is where REGISTERs go: -pbx-registrar if given, otherwise the
// trunk peer itself, which is the usual case. The transport always follows
// the trunk — one leg, one listener, one transport.
func registrarURI(spec string, trunk *pbx.Trunk) (sip.Uri, error) {
	host, port := trunk.Host, trunk.Port
	if spec = strings.TrimSpace(spec); spec != "" {
		spec = strings.TrimPrefix(spec, "sip:")
		h, p, err := net.SplitHostPort(spec)
		if err != nil {
			host = strings.Trim(spec, "[]") // no port given
		} else {
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 || n > 65535 {
				return sip.Uri{}, fmt.Errorf("bad -pbx-registrar port in %q", spec)
			}
			host, port = h, n
		}
		if host == "" {
			return sip.Uri{}, fmt.Errorf("bad -pbx-registrar %q", spec)
		}
	}
	u := sip.Uri{Scheme: "sip", Host: host, Port: port, UriParams: sip.NewParams()}
	u.UriParams.Add("transport", strings.ToLower(trunk.Transport))
	return u, nil
}

// startPBXLines builds the line registrar and hands it to the call server.
//
// It must run before calls.Serve: the server reads the registry from the
// call path, and Serve is both what begins serving calls and what starts the
// registration loops (b2bua.startLines).
//
// The manager is returned so the caller can Stop it, which drops every
// binding on the way out rather than leaving the exchange ringing contacts
// this server has stopped listening on.
func startPBXLines(log *slog.Logger, o options, trunk *pbx.Trunk, calls *b2bua.Server, devices *enroll.Store) (*pbxline.Manager, error) {
	if trunk == nil {
		return nil, fmt.Errorf("-pbx-mode=lines needs a PBX to register to: give -trunk")
	}
	client := calls.TrunkClient()
	if client == nil {
		return nil, fmt.Errorf("-pbx-mode=lines: no SIP client for the PBX leg")
	}
	contact, ok := calls.TrunkContact()
	if !ok {
		return nil, fmt.Errorf("-pbx-mode=lines: cannot work out the address the PBX should call us back on")
	}
	target, err := registrarURI(o.pbxRegistrar, trunk)
	if err != nil {
		return nil, err
	}
	domain := o.pbxDomain
	if domain == "" {
		domain = trunk.Host
	}
	registrar, err := pbxline.NewSIPRegistrar(pbxline.SIPConfig{
		Client:    client,
		Registrar: target,
		Domain:    domain,
		Contact:   contact,
		UserAgent: "dialler",
	})
	if err != nil {
		return nil, err
	}

	mgr := pbxline.New(pbxline.Config{
		Registrar: registrar,
		Expiry:    o.pbxExpiry,
		Log:       log,
	})
	// Every line the device store holds, minus the revoked ones.
	creds, err := devices.PBXCredentials()
	if err != nil {
		// Not fatal: one unreadable record must not keep the rest of the
		// fleet off the exchange. The error names the device.
		log.Error("some PBX line credentials could not be read; those lines will not register", "err", err)
	}
	mgr.Set(linesFrom(creds))
	calls.SetLines(mgr)

	log.Info("pbx mode: lines", "registrar", target.String(), "domain", domain,
		"contact", contact.String(), "lines", len(creds), "expiry", o.pbxExpiry,
		"default_line", o.pbxDefaultLine, "peers", o.pbxPeers)
	if len(creds) == 0 {
		log.Warn("-pbx-mode=lines but no device has a PBX line configured; no call will reach the exchange until one does (PUT /v1/admin/devices/{id}/pbx-line)")
	}
	return mgr, nil
}

func linesFrom(creds []enroll.PBXCredential) []pbxline.Line {
	out := make([]pbxline.Line, 0, len(creds))
	for _, c := range creds {
		out = append(out, pbxline.Line{User: c.User, DN: c.DN, DigestUser: c.DigestUser, Secret: c.Secret})
	}
	return out
}

// pbxLineHook keeps the registrar in step with the admin API: a line written
// registers now and a line removed drops its binding, one device at a time
// and with nothing reloaded (SPEC §4.8).
func pbxLineHook(log *slog.Logger, mgr *pbxline.Manager, devices *enroll.Store) func(string, *enroll.PBXCredential) {
	if mgr == nil {
		// Trunk mode: an administrator may still provision lines against a
		// server that is not using them, ready for a later switch.
		return nil
	}
	return func(deviceID string, cred *enroll.PBXCredential) {
		if cred == nil {
			if user, ok := devices.UserFor(deviceID); ok {
				log.Info("pbx line removed", "device", deviceID, "user", user)
				mgr.Delete(user)
			}
			return
		}
		log.Info("pbx line set", "device", deviceID, "user", cred.User, "dn", cred.DN, "digest_user", cred.DigestUser)
		mgr.Put(pbxline.Line{User: cred.User, DN: cred.DN, DigestUser: cred.DigestUser, Secret: cred.Secret})
	}
}
