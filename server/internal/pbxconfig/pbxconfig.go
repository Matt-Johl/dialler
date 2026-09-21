// Package pbxconfig stores the PBX settings an administrator sets in the
// admin UI (SPEC §4.8, §6 item 3): how the call server reaches the PBX
// and in which mode. Two modes: peer, where the PBX trusts the server by
// address (Asterisk, today's behaviour), and register, where the server
// REGISTERs with the PBX as a third-party SIP device for every extension
// that holds credentials (CUCM's device model).
//
// This package is the store and the admin route only. Saved settings are
// read by the call server at start-up in place of its flags; the
// registration manager that acts on register mode is separate work.
package pbxconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"dialler/server/internal/enroll"
)

// Mode is how the PBX knows the server.
type Mode string

const (
	// ModePeer: the PBX trusts the server's address; calls to app extensions
	// arrive over one trunk.
	ModePeer Mode = "peer"
	// ModeRegister: the server registers each extension with the PBX as a
	// third-party SIP device, with that extension's digest credentials.
	ModeRegister Mode = "register"
)

// Settings is the PBX as the administrator described it.
type Settings struct {
	Mode      Mode   `json:"mode"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Transport string `json:"transport"` // udp | tcp | tls
	// ExpirySeconds is the registration lifetime asked for in register mode.
	ExpirySeconds int `json:"expiry_seconds"`
	// Codecs offered to the PBX, in order of preference (g722,pcmu,pcma,opus).
	Codecs string `json:"codecs"`
	// SRTP on the PBX leg: off | sdes.
	SRTP string `json:"srtp"`
	// QualifySeconds is the OPTIONS keep-alive interval; 0 turns it off.
	QualifySeconds int `json:"qualify_seconds"`
	// TLS material for a TLS trunk, as paths on the server.
	TLSCert     string `json:"tls_cert,omitempty"`
	TLSKey      string `json:"tls_key,omitempty"`
	TLSCA       string `json:"tls_ca,omitempty"`
	TLSInsecure bool   `json:"tls_insecure,omitempty"`

	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ErrInvalid carries what was wrong with a submitted Settings.
type ErrInvalid struct{ Reason string }

func (e *ErrInvalid) Error() string { return "pbx settings: " + e.Reason }

// Validate checks and normalises s in place.
func (s *Settings) Validate() error {
	s.Host = strings.TrimSpace(s.Host)
	s.Transport = strings.ToLower(strings.TrimSpace(s.Transport))
	s.SRTP = strings.ToLower(strings.TrimSpace(s.SRTP))
	s.Codecs = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s.Codecs), " ", ""))
	switch s.Mode {
	case ModePeer, ModeRegister:
	default:
		return &ErrInvalid{"mode must be peer or register"}
	}
	if s.Host == "" {
		return &ErrInvalid{"the PBX host is required"}
	}
	switch s.Transport {
	case "udp", "tcp", "tls":
	case "":
		s.Transport = "udp"
	default:
		return &ErrInvalid{"transport must be udp, tcp or tls"}
	}
	if s.Port == 0 {
		s.Port = 5060
		if s.Transport == "tls" {
			s.Port = 5061
		}
	}
	if s.Port < 1 || s.Port > 65535 {
		return &ErrInvalid{"port must be between 1 and 65535"}
	}
	if s.ExpirySeconds == 0 {
		s.ExpirySeconds = 300
	}
	if s.ExpirySeconds < 60 || s.ExpirySeconds > 86400 {
		return &ErrInvalid{"registration expiry must be between 60 and 86400 seconds"}
	}
	if s.Codecs == "" {
		s.Codecs = "g722,pcmu,pcma"
	}
	for _, c := range strings.Split(s.Codecs, ",") {
		switch c {
		case "g722", "pcmu", "pcma", "opus":
		default:
			return &ErrInvalid{fmt.Sprintf("codec %q is not g722, pcmu, pcma or opus", c)}
		}
	}
	switch s.SRTP {
	case "off", "sdes":
	case "":
		s.SRTP = "off"
	default:
		return &ErrInvalid{"srtp must be off or sdes"}
	}
	if s.QualifySeconds < 0 || s.QualifySeconds > 3600 {
		return &ErrInvalid{"qualify interval must be between 0 and 3600 seconds"}
	}
	if s.Transport != "tls" && (s.TLSCert != "" || s.TLSKey != "" || s.TLSCA != "" || s.TLSInsecure) {
		return &ErrInvalid{"TLS settings only apply with the tls transport"}
	}
	return nil
}

// Store keeps the settings in one JSON file.
type Store struct {
	mu   sync.RWMutex
	path string
	cur  *Settings
	now  func() time.Time
}

// Open loads path (memory-only if ""); no file means nothing set yet.
func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var st Settings
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("pbx settings %s: %w", path, err)
	}
	s.cur = &st
	return s, nil
}

// Get returns the saved settings, or nil when none have been saved.
func (s *Store) Get() *Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cur == nil {
		return nil
	}
	cp := *s.cur
	return &cp
}

// Set validates and saves st, bumping its version.
func (s *Store) Set(st Settings) (Settings, error) {
	if err := st.Validate(); err != nil {
		return Settings{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		st.Version = s.cur.Version + 1
	} else {
		st.Version = 1
	}
	st.UpdatedAt = s.now()
	if s.path != "" {
		raw, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return Settings{}, err
		}
		if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
			return Settings{}, err
		}
		tmp := s.path + ".tmp"
		if err := os.WriteFile(tmp, raw, 0o600); err != nil {
			return Settings{}, err
		}
		if err := os.Rename(tmp, s.path); err != nil {
			return Settings{}, err
		}
	}
	s.cur = &st
	return st, nil
}

// Handler serves the admin route behind the admin bearer token:
//
//	GET /v1/admin/pbx  → Settings (404 until set)
//	PUT /v1/admin/pbx  Settings → Settings
//
// onChange, if set, is told after a save (the call server logs that a
// restart applies it; a future registration manager reconciles live).
func Handler(store *Store, adminToken string, onChange func(Settings)) http.Handler {
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || !enroll.BearerMatches(r, adminToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /v1/admin/pbx", guard(func(w http.ResponseWriter, r *http.Request) {
		st := store.Get()
		if st == nil {
			http.Error(w, "no PBX settings saved", http.StatusNotFound)
			return
		}
		writeJSON(w, st)
	}))
	mux.HandleFunc("PUT /v1/admin/pbx", guard(func(w http.ResponseWriter, r *http.Request) {
		var in Settings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		st, err := store.Set(in)
		var inv *ErrInvalid
		if errors.As(err, &inv) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if onChange != nil {
			onChange(st)
		}
		writeJSON(w, st)
	}))
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
