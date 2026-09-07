package sip

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"dialler/server/internal/registry"
)

const register = "REGISTER sip:dialler.example.local SIP/2.0\r\n" +
	"Via: SIP/2.0/TLS 10.0.0.5:5061;branch=z9hG4bK776asdhds;rport\r\n" +
	"Max-Forwards: 70\r\n" +
	"To: \"Matt\" <sip:201@dialler.example.local>\r\n" +
	"From: \"Matt\" <sip:201@dialler.example.local>;tag=456248\r\n" +
	"Call-ID: 843817637684230@10.0.0.5\r\n" +
	"CSeq: 1826 REGISTER\r\n" +
	"Contact: <sip:201@10.0.0.5:5061;transport=tls>;expires=300\r\n" +
	"Expires: 7200\r\n" +
	"Content-Length: 0\r\n\r\n"

func TestParseAndSerialise(t *testing.T) {
	m, err := Parse([]byte(register))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsRequest || m.Method != "REGISTER" || m.RequestURI != "sip:dialler.example.local" {
		t.Fatalf("start line: %+v", m)
	}
	if got := m.Get("call-id"); got != "843817637684230@10.0.0.5" {
		t.Fatalf("case-insensitive Get: %q", got)
	}
	if n, meth := m.CSeq(); n != 1826 || meth != "REGISTER" {
		t.Fatalf("CSeq: %d %s", n, meth)
	}
	if ContactExpires(m, 60) != 300 {
		t.Fatal("Contact expires param should win over Expires header")
	}
	if URIUser(m.Get("To")) != "201" || URIHost(m.Get("To")) != "dialler.example.local" {
		t.Fatalf("URI parsing: %q %q", URIUser(m.Get("To")), URIHost(m.Get("To")))
	}
	// Round trip preserves header order and body length.
	m.Body = []byte("v=0\r\n")
	again, err := Parse(m.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Body, m.Body) || again.Get("Content-Length") != "5" {
		t.Fatalf("body round trip: %q %q", again.Body, again.Get("Content-Length"))
	}
	if len(again.Headers) != len(m.Headers) {
		t.Fatalf("header count drift %d vs %d", len(again.Headers), len(m.Headers))
	}
}

func TestCompactHeadersAndKeepAlive(t *testing.T) {
	raw := "\r\n\r\nSIP/2.0 200 OK\r\nv: SIP/2.0/TLS a;branch=b\r\ni: abc\r\nl: 3\r\n\r\nabc"
	m, err := ReadMessage(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if m.IsRequest || m.StatusCode != 200 || m.Get("Via") == "" || m.Get("Call-ID") != "abc" || string(m.Body) != "abc" {
		t.Fatalf("compact parse: %+v", m)
	}
	if _, err := Parse([]byte("GARBAGE\r\n\r\n")); err == nil {
		t.Fatal("garbage accepted")
	}
	if _, err := Parse([]byte("SIP/2.0 200 OK\r\nl: 10\r\n\r\nshort")); err == nil {
		t.Fatal("truncated body accepted")
	}
}

func TestURIUserShapes(t *testing.T) {
	cases := map[string]string{
		"sip:201@h":                    "201",
		"sips:201@h;transport=tls":     "201",
		"<sip:201@h>":                  "201",
		"\"N\" <sip:201@h:5061>;tag=1": "201",
		"201":                          "201",
		"sip:+441onal@h":               "+441onal",
		"sip:h":                        "h", // host-only URI has no user; caller decides
		"tel:+1234":                    "tel:+1234",
	}
	for in, want := range cases {
		if got := URIUser(in); got != want {
			t.Errorf("URIUser(%q) = %q want %q", in, got, want)
		}
	}
	if URIHost("201") != "" || URIHost("<sip:201@h:5061>;tag=1") != "h:5061" {
		t.Fatal("URIHost")
	}
}

func TestSetAddDel(t *testing.T) {
	m := &Message{}
	m.Add("Via", "a")
	m.Add("v", "b")
	if len(m.Values("Via")) != 2 {
		t.Fatal("compact Add not merged")
	}
	m.Set("Via", "c")
	if vs := m.Values("Via"); len(vs) != 1 || vs[0] != "c" {
		t.Fatalf("Set: %v", vs)
	}
	m.Del("via")
	if m.Get("Via") != "" {
		t.Fatal("Del")
	}
}

// startRegistrar drives HandleConn over net.Pipe: no sockets needed, so the
// suite runs in any sandbox. Serve() is a thin accept loop over HandleConn.
func startRegistrar(t *testing.T, reg *registry.Registry) (*Registrar, net.Conn) {
	t.Helper()
	r := &Registrar{Registry: reg, Domain: "dialler.example.local", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	cs, ss := net.Pipe()
	done := make(chan struct{})
	go func() { r.HandleConn(ctx, ss); close(done) }()
	t.Cleanup(func() { _ = cs.Close(); cancel(); <-done })
	return r, cs
}

func roundTrip(t *testing.T, c net.Conn, br *bufio.Reader, req string) *Message {
	t.Helper()
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	resp, err := ReadMessage(br)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRegistrarFlow(t *testing.T) {
	reg := registry.New(nil)
	reg.Provision("201", "dev1")
	_, c := startRegistrar(t, reg)
	br := bufio.NewReader(c)

	resp := roundTrip(t, c, br, register)
	if resp.StatusCode != 200 {
		t.Fatalf("REGISTER: %d %s", resp.StatusCode, resp.Reason)
	}
	if !strings.Contains(resp.Get("Contact"), ";expires=300") || resp.Get("Expires") != "300" {
		t.Fatalf("expires not echoed: %q %q", resp.Get("Contact"), resp.Get("Expires"))
	}
	if !strings.Contains(resp.Get("To"), ";tag=") {
		t.Fatal("no To tag")
	}
	ep, _ := reg.Lookup("201")
	if ep.Contact != "<sip:201@10.0.0.5:5061;transport=tls>" {
		t.Fatalf("stored contact %q", ep.Contact)
	}

	// Too-brief interval.
	brief := strings.Replace(register, "expires=300", "expires=5", 1)
	if resp := roundTrip(t, c, br, brief); resp.StatusCode != 423 || resp.Get("Min-Expires") != "60" {
		t.Fatalf("brief: %d %q", resp.StatusCode, resp.Get("Min-Expires"))
	}

	// Unknown user and wrong domain.
	unknown := strings.ReplaceAll(register, "201@", "999@")
	if resp := roundTrip(t, c, br, unknown); resp.StatusCode != 404 {
		t.Fatalf("unknown user: %d", resp.StatusCode)
	}
	foreign := strings.Replace(register, "<sip:201@dialler.example.local>", "<sip:201@evil.example>", 1)
	if resp := roundTrip(t, c, br, foreign); resp.StatusCode != 403 {
		t.Fatalf("foreign domain: %d", resp.StatusCode)
	}

	// OPTIONS and an unsupported method on the same connection.
	options := strings.Replace(strings.Replace(register, "REGISTER sip:", "OPTIONS sip:", 1), "1826 REGISTER", "1827 OPTIONS", 1)
	if resp := roundTrip(t, c, br, options); resp.StatusCode != 200 {
		t.Fatalf("OPTIONS: %d", resp.StatusCode)
	}
	invite := strings.Replace(strings.Replace(register, "REGISTER sip:", "INVITE sip:", 1), "1826 REGISTER", "1828 INVITE", 1)
	if resp := roundTrip(t, c, br, invite); resp.StatusCode != 501 {
		t.Fatalf("INVITE before B2BUA: %d", resp.StatusCode)
	}

	// Keep-alive CRLF is tolerated; unregister with expires=0.
	_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = c.Write([]byte("\r\n\r\n"))
	bye := strings.Replace(register, "expires=300", "expires=0", 1)
	if resp := roundTrip(t, c, br, bye); resp.StatusCode != 200 {
		t.Fatalf("unregister: %d", resp.StatusCode)
	}
	if reg.Registered("201") {
		t.Fatal("still registered after expires=0")
	}
}
