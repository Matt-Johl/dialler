package sip

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"dialler/server/internal/registry"
)

// Registrar answers REGISTER and OPTIONS on the app leg and keeps the
// registry current. Every other request is refused with 501. This is the
// Phase 0 fallback/reference registrar: the live one is in internal/b2bua,
// which also authenticates the app leg with SIP Digest (internal/sipauth);
// this one has no authentication and is not on the call path.
type Registrar struct {
	Registry   *registry.Registry
	Domain     string        // served domain, e.g. "dialler.example.local"
	MinExpires int           // seconds; default 60
	MaxExpires int           // seconds; default 3600
	Logger     *slog.Logger  // default slog.Default()
	ReadIdle   time.Duration // close idle connections; default 5m

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

func (r *Registrar) defaults() {
	if r.MinExpires == 0 {
		r.MinExpires = 60
	}
	if r.MaxExpires == 0 {
		r.MaxExpires = 3600
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.ReadIdle == 0 {
		r.ReadIdle = 5 * time.Minute
	}
}

// Serve accepts stream connections (TLS in production) until ctx ends.
func (r *Registrar) Serve(ctx context.Context, l net.Listener) error {
	r.defaults()
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("sip: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.HandleConn(ctx, c)
		}()
	}
}

// HandleConn serves one stream connection until it closes.
func (r *Registrar) HandleConn(ctx context.Context, c net.Conn) {
	r.defaults()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	defer c.Close()
	br := bufio.NewReader(c)
	log := r.Logger.With("remote", c.RemoteAddr().String())
	for {
		_ = c.SetReadDeadline(time.Now().Add(r.ReadIdle))
		m, err := ReadMessage(br)
		if err != nil {
			if err != io.EOF {
				log.Debug("sip read", "err", err)
			}
			return
		}
		if !m.IsRequest {
			continue // no client transactions yet; ignore stray responses
		}
		resp := r.handle(m, c.RemoteAddr())
		_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Write(resp.Bytes()); err != nil {
			return
		}
	}
}

func (r *Registrar) handle(req *Message, remote net.Addr) *Message {
	switch req.Method {
	case "REGISTER":
		return r.register(req, remote)
	case "OPTIONS":
		resp := NewResponse(req, 200, "OK")
		resp.Set("Allow", "REGISTER, OPTIONS")
		return resp
	default:
		resp := NewResponse(req, 501, "Not Implemented")
		resp.Set("Allow", "REGISTER, OPTIONS")
		return resp
	}
}

func (r *Registrar) register(req *Message, remote net.Addr) *Message {
	user := URIUser(req.Get("To"))
	if user == "" {
		return NewResponse(req, 400, "Bad Request")
	}
	if r.Domain != "" {
		host := URIHost(req.Get("To"))
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != "" && !strings.EqualFold(host, r.Domain) {
			return NewResponse(req, 403, "Forbidden")
		}
	}
	contact := req.Get("Contact")
	if contact == "" {
		// Query: report current binding.
		ep, ok := r.Registry.Lookup(user)
		if !ok {
			return NewResponse(req, 404, "Not Found")
		}
		resp := NewResponse(req, 200, "OK")
		if ep.Contact != "" {
			resp.Add("Contact", ep.Contact)
		}
		return resp
	}
	expires := ContactExpires(req, r.MaxExpires)
	if contact == "*" || expires == 0 {
		r.Registry.Unregister(user)
		r.Logger.Info("sip unregister", "user", user, "remote", remote.String())
		return NewResponse(req, 200, "OK")
	}
	if expires < r.MinExpires {
		resp := NewResponse(req, 423, "Interval Too Brief")
		resp.Set("Min-Expires", strconv.Itoa(r.MinExpires))
		return resp
	}
	if expires > r.MaxExpires {
		expires = r.MaxExpires
	}
	// Store the contact without its expires param; we report our own.
	stored := contact
	if i := strings.Index(strings.ToLower(stored), ";expires="); i >= 0 {
		end := strings.IndexByte(stored[i+1:], ';')
		if end < 0 {
			stored = stored[:i]
		} else {
			stored = stored[:i] + stored[i+1+end:]
		}
	}
	ep, err := r.Registry.Register(user, stored, time.Duration(expires)*time.Second)
	if err != nil {
		return NewResponse(req, 404, "Not Found")
	}
	r.Logger.Info("sip register", "user", user, "contact", ep.Contact, "expires", expires, "remote", remote.String())
	resp := NewResponse(req, 200, "OK")
	resp.Add("Contact", fmt.Sprintf("%s;expires=%d", stored, expires))
	resp.Set("Expires", strconv.Itoa(expires))
	return resp
}
