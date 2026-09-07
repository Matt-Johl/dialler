// Package directory is the server-side address book with delta sync
// (SPEC §5 component 4). Every change bumps a monotonic version; clients
// pull everything newer than the version they hold, including tombstones,
// so a single "since" cursor gives them a consistent view.
package directory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Mode says how a contact is reached; the app UI is identical for both.
type Mode string

const (
	ModeLocal Mode = "local" // app endpoint on this server
	ModeTrunk Mode = "trunk" // via the PBX trunk
)

// Contact is one address-book entry.
type Contact struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	URI         string    `json:"uri"`
	Mode        Mode      `json:"mode"`
	Version     int64     `json:"version"`
	UpdatedAt   time.Time `json:"updated_at"`
	Deleted     bool      `json:"deleted,omitempty"`
}

// ErrInvalid is returned for contacts missing a URI or a valid mode.
var ErrInvalid = errors.New("directory: uri and mode (local|trunk) are required")

type state struct {
	Version  int64               `json:"version"`
	Contacts map[string]*Contact `json:"contacts"`
}

// Store is safe for concurrent use.
type Store struct {
	mu       sync.RWMutex
	path     string
	st       state
	now      func() time.Time
	onChange func(int64)
}

// Open loads the store at path (memory-only if ""), creating it on first save.
func Open(path string) (*Store, error) {
	s := &Store{path: path, st: state{Contacts: map[string]*Contact{}}, now: time.Now}
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
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.st); err != nil {
			return nil, err
		}
		if s.st.Contacts == nil {
			s.st.Contacts = map[string]*Contact{}
		}
	}
	return s, nil
}

// OnChange registers a callback invoked (outside the lock) after each change.
func (s *Store) OnChange(fn func(version int64)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Version is the current directory version.
func (s *Store) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.st.Version
}

// Upsert creates or replaces a contact. An empty ID is assigned.
func (s *Store) Upsert(c Contact) (Contact, error) {
	if c.URI == "" || (c.Mode != ModeLocal && c.Mode != ModeTrunk) {
		return Contact{}, ErrInvalid
	}
	s.mu.Lock()
	if c.ID == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		c.ID = "ct_" + hex.EncodeToString(b[:])
	}
	s.st.Version++
	c.Version = s.st.Version
	c.UpdatedAt = s.now()
	c.Deleted = false
	cp := c
	s.st.Contacts[c.ID] = &cp
	err := s.saveLocked()
	v, fn := s.st.Version, s.onChange
	s.mu.Unlock()
	if err != nil {
		return Contact{}, err
	}
	if fn != nil {
		fn(v)
	}
	return c, nil
}

// Delete tombstones a contact. Returns false if unknown or already deleted.
func (s *Store) Delete(id string) (bool, error) {
	s.mu.Lock()
	c, ok := s.st.Contacts[id]
	if !ok || c.Deleted {
		s.mu.Unlock()
		return false, nil
	}
	s.st.Version++
	c.Deleted = true
	c.Version = s.st.Version
	c.UpdatedAt = s.now()
	err := s.saveLocked()
	v, fn := s.st.Version, s.onChange
	s.mu.Unlock()
	if err != nil {
		return false, err
	}
	if fn != nil {
		fn(v)
	}
	return true, nil
}

// Changes returns every contact changed after since, ordered by version,
// plus the current version. since == 0 is a full sync and omits tombstones.
func (s *Store) Changes(since int64) ([]Contact, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Contact{}
	for _, c := range s.st.Contacts {
		if c.Version <= since {
			continue
		}
		if since == 0 && c.Deleted {
			continue
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, s.st.Version
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
