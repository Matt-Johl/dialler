// Command fake-app is the headless stand-in for the iOS app's background
// wake path (SPEC §7.1 signal-transport seam, automated sibling). It holds a
// wire-protocol connection to the gateway as an enrolled device, and when a
// wake arrives it does what the real app does: acknowledges it, then brings
// the SIP user agent up so the pending call can be bridged. Here "bringing
// the UA up" means telling a headless baresip to register via its ctrl_tcp
// interface.
//
// Exit 0 once a wake has been handled; exit 1 on timeout or protocol error.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"dialler/server/internal/wire"
)

func main() {
	var (
		server   = flag.String("server", "dialler:7443", "gateway host:port")
		deviceID = flag.String("device", "", "enrolled device id")
		token    = flag.String("token", "", "device token from enrolment")
		kind     = flag.String("kind", "extension", "connection kind: app | extension")
		phoneCtl = flag.String("phone-ctl", "", "baresip ctrl_tcp host:port to register on wake (optional)")
		wakeCmd  = flag.String("wake-cmd", `{"command":"reginfo"}`, "ctrl_tcp JSON command sent on wake (the wake test passes a uanew that registers)")
		timeout  = flag.Duration("timeout", 60*time.Second, "give up waiting for a wake after this long")
		hold     = flag.Duration("hold", 15*time.Second, "keep the connection open this long after handling the wake")
	)
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *deviceID == "" || *token == "" {
		log.Error("-device and -token are required")
		os.Exit(2)
	}

	conn, err := tls.Dial("tcp", *server, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // dev cert
	if err != nil {
		log.Error("dial", "err", err)
		os.Exit(1)
	}
	defer conn.Close()

	send := func(t wire.Type, body any) {
		e, err := wire.New(t, fmt.Sprintf("fake-%d", time.Now().UnixNano()), time.Now(), body)
		if err == nil {
			err = wire.WriteFrame(conn, e)
		}
		if err != nil {
			log.Error("send", "type", t, "err", err)
			os.Exit(1)
		}
	}

	send(wire.TypeHello, wire.Hello{DeviceID: *deviceID, Token: *token, Client: wire.ClientKind(*kind), AppVersion: "fake-app"})
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	e, err := wire.ReadFrame(conn)
	if err != nil || e.Type != wire.TypeWelcome {
		log.Error("no welcome", "type", e.Type, "body", string(e.Body), "err", err)
		os.Exit(1)
	}
	var w wire.Welcome
	_ = e.DecodeBody(&w)
	log.Info("connected", "session", w.SessionID, "heartbeat_s", w.HeartbeatSeconds)

	// Heartbeat.
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Duration(w.HeartbeatSeconds) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				send(wire.TypePing, nil)
			case <-stop:
				return
			}
		}
	}()

	deadline := time.Now().Add(*timeout)
	for {
		_ = conn.SetReadDeadline(deadline)
		e, err := wire.ReadFrame(conn)
		if err != nil {
			log.Error("waiting for wake", "err", err)
			os.Exit(1)
		}
		switch e.Type {
		case wire.TypeWake:
			var wk wire.Wake
			_ = e.DecodeBody(&wk)
			out, _ := json.Marshal(wk)
			fmt.Println(string(out)) // stdout: the wake, for the test script
			log.Info("wake received", "call", wk.CallID, "from", wk.From.URI, "sip", fmt.Sprintf("%s:%d/%s", wk.SIP.Host, wk.SIP.Port, wk.SIP.Transport))
			send(wire.TypeWakeAck, wire.WakeAck{CallID: wk.CallID, Action: wire.WakeWillAnswer})
			if *phoneCtl != "" {
				if err := ctrlTCP(*phoneCtl, *wakeCmd); err != nil {
					log.Error("register phone on wake", "err", err)
					os.Exit(1)
				}
				log.Info("phone told to register", "ctl", *phoneCtl)
			}
			// Stay connected like the extension would, then exit cleanly.
			time.Sleep(*hold)
			close(stop)
			return
		case wire.TypeWakeCancel:
			log.Warn("wake cancelled before we could act", "body", string(e.Body))
		case wire.TypeError:
			log.Error("gateway error", "body", string(e.Body))
			os.Exit(1)
		default:
			// pong, directory_changed, …
		}
	}
}

// ctrlTCP sends one netstring-framed JSON command to baresip's ctrl_tcp.
func ctrlTCP(addr, cmd string) error {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = fmt.Fprintf(c, "%d:%s,", len(cmd), cmd)
	if err != nil {
		return err
	}
	buf := make([]byte, 512)
	_, _ = c.Read(buf) // response; best effort
	return nil
}
