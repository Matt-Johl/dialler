// Package sip is a minimal, dependency-free SIP message layer (RFC 3261
// syntax subset) plus the stream transport and registrar used on the app leg.
// It is intentionally small: the Phase 0 engine spike decides whether the
// B2BUA is built on top of this or on an embedded engine (SPEC §6).
package sip

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Header is one header line. Order is preserved on the wire.
type Header struct {
	Name  string
	Value string
}

// Message is a request or a response.
type Message struct {
	IsRequest  bool
	Method     string // requests
	RequestURI string // requests
	StatusCode int    // responses
	Reason     string // responses
	Headers    []Header
	Body       []byte
}

// compact-form header names (RFC 3261 §7.3.3) → canonical.
var compact = map[string]string{
	"v": "Via", "f": "From", "t": "To", "i": "Call-ID", "m": "Contact",
	"l": "Content-Length", "c": "Content-Type", "s": "Subject", "k": "Supported", "e": "Content-Encoding",
}

// Errors.
var (
	ErrBadStartLine = errors.New("sip: malformed start line")
	ErrBadHeader    = errors.New("sip: malformed header")
)

// canonicalName normalises header names for case-insensitive lookup and
// expands compact forms.
func canonicalName(n string) string {
	n = strings.TrimSpace(n)
	if c, ok := compact[strings.ToLower(n)]; ok {
		return c
	}
	return n
}

// Get returns the first value of the named header ("" if absent).
func (m *Message) Get(name string) string {
	want := canonicalName(name)
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, want) {
			return h.Value
		}
	}
	return ""
}

// Values returns all values of the named header.
func (m *Message) Values(name string) []string {
	want := canonicalName(name)
	var out []string
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, want) {
			out = append(out, h.Value)
		}
	}
	return out
}

// Set replaces every instance of the header with a single value.
func (m *Message) Set(name, value string) {
	want := canonicalName(name)
	kept := m.Headers[:0]
	placed := false
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, want) {
			if !placed {
				kept = append(kept, Header{want, value})
				placed = true
			}
			continue
		}
		kept = append(kept, h)
	}
	if !placed {
		kept = append(kept, Header{want, value})
	}
	m.Headers = kept
}

// Add appends a header instance.
func (m *Message) Add(name, value string) {
	m.Headers = append(m.Headers, Header{canonicalName(name), value})
}

// Del removes every instance of the header.
func (m *Message) Del(name string) {
	want := canonicalName(name)
	kept := m.Headers[:0]
	for _, h := range m.Headers {
		if !strings.EqualFold(h.Name, want) {
			kept = append(kept, h)
		}
	}
	m.Headers = kept
}

// CSeq returns the sequence number and method from the CSeq header.
func (m *Message) CSeq() (int, string) {
	f := strings.Fields(m.Get("CSeq"))
	if len(f) != 2 {
		return 0, ""
	}
	n, _ := strconv.Atoi(f[0])
	return n, f[1]
}

// Bytes serialises the message, always emitting a correct Content-Length.
func (m *Message) Bytes() []byte {
	var b bytes.Buffer
	if m.IsRequest {
		fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", m.Method, m.RequestURI)
	} else {
		fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", m.StatusCode, m.Reason)
	}
	for _, h := range m.Headers {
		if strings.EqualFold(h.Name, "Content-Length") {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", h.Name, h.Value)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(m.Body))
	b.Write(m.Body)
	return b.Bytes()
}

// Parse decodes one complete message from raw bytes.
func Parse(raw []byte) (*Message, error) {
	return ReadMessage(bufio.NewReader(bytes.NewReader(raw)))
}

// ReadMessage reads exactly one message from a stream transport. It skips
// CRLF keep-alive pairs between messages (RFC 5626 §4.4.1).
func ReadMessage(r *bufio.Reader) (*Message, error) {
	var start string
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			continue // keep-alive / stray CRLF
		}
		start = line
		break
	}
	m := &Message{}
	switch {
	case strings.HasPrefix(start, "SIP/2.0 "):
		parts := strings.SplitN(start, " ", 3)
		if len(parts) < 2 {
			return nil, ErrBadStartLine
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil || code < 100 || code > 699 {
			return nil, ErrBadStartLine
		}
		m.StatusCode = code
		if len(parts) == 3 {
			m.Reason = parts[2]
		}
	default:
		parts := strings.SplitN(start, " ", 3)
		if len(parts) != 3 || parts[2] != "SIP/2.0" {
			return nil, ErrBadStartLine
		}
		m.IsRequest = true
		m.Method = parts[0]
		m.RequestURI = parts[1]
	}

	contentLength := -1
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		// Header folding (continuation lines) is obsolete; treat as error.
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%w: %q", ErrBadHeader, line)
		}
		h := Header{canonicalName(name), strings.TrimSpace(value)}
		if strings.EqualFold(h.Name, "Content-Length") {
			n, err := strconv.Atoi(h.Value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%w: bad Content-Length", ErrBadHeader)
			}
			contentLength = n
		}
		m.Headers = append(m.Headers, h)
	}
	if contentLength < 0 {
		contentLength = 0 // stream transports require it; tolerate absence as empty
	}
	if contentLength > 0 {
		m.Body = make([]byte, contentLength)
		if _, err := io.ReadFull(r, m.Body); err != nil {
			return nil, fmt.Errorf("sip: read body: %w", err)
		}
	}
	return m, nil
}

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF && line == "" {
			return "", io.EOF
		}
		if err == io.EOF {
			return "", io.ErrUnexpectedEOF
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// NewResponse builds a response to req with the mandatory headers copied
// (Via, From, To, Call-ID, CSeq). A To-tag is added when missing.
func NewResponse(req *Message, code int, reason string) *Message {
	resp := &Message{StatusCode: code, Reason: reason}
	for _, v := range req.Values("Via") {
		resp.Add("Via", v)
	}
	resp.Add("From", req.Get("From"))
	to := req.Get("To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + RandomToken(8)
	}
	resp.Add("To", to)
	resp.Add("Call-ID", req.Get("Call-ID"))
	resp.Add("CSeq", req.Get("CSeq"))
	return resp
}

// RandomToken returns n random bytes as hex (for tags and branches).
func RandomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// URIUser extracts the user part from a SIP address in any of the common
// shapes: "sip:201@host", "sips:201@host;p=1", "<sip:201@host>",
// "\"Name\" <sip:201@host>;tag=x", or a bare "201".
func URIUser(addr string) string {
	s := strings.TrimSpace(addr)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = s[i+1:]
		if j := strings.IndexByte(s, '>'); j >= 0 {
			s = s[:j]
		}
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "sips:"), "sip:")
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexAny(s, ";?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// URIHost extracts the host[:port] from a SIP address ("" if none).
func URIHost(addr string) string {
	s := strings.TrimSpace(addr)
	if i := strings.IndexByte(s, '<'); i >= 0 {
		s = s[i+1:]
		if j := strings.IndexByte(s, '>'); j >= 0 {
			s = s[:j]
		}
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "sips:"), "sip:")
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	} else if !strings.Contains(s, ".") && !strings.Contains(s, ":") {
		return "" // bare user
	}
	if i := strings.IndexAny(s, ";?"); i >= 0 {
		s = s[:i]
	}
	return s
}

// ContactExpires returns the expiry requested by a REGISTER: the Contact
// "expires" param if present, else the Expires header, else def.
func ContactExpires(req *Message, def int) int {
	c := req.Get("Contact")
	for _, p := range strings.Split(c, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if ok && strings.EqualFold(k, "expires") {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	if e := req.Get("Expires"); e != "" {
		if n, err := strconv.Atoi(e); err == nil {
			return n
		}
	}
	return def
}
