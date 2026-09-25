// Package directory is the server-side address book with delta sync
// (SPEC §5 component 4). Since SPEC §6 item 7 there is one directory per
// device: each has its own monotonic version and its own file, so a change
// to one device's list touches nothing of another's. Every change bumps
// that directory's version; a client pulls everything newer than the
// version it holds, including tombstones, so a single "since" cursor gives
// it a consistent view.
package directory

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	Favourite   bool      `json:"favourite,omitempty"`
	Version     int64     `json:"version"`
	UpdatedAt   time.Time `json:"updated_at"`
	Deleted     bool      `json:"deleted,omitempty"`
}

// ErrInvalid is returned for contacts missing a URI or a valid mode.
var ErrInvalid = errors.New("directory: uri and mode (local|trunk) are required")

// ErrNoDevice is returned for an empty device id.
var ErrNoDevice = errors.New("directory: device id is required")

// ErrVersionMismatch is returned when a Match precondition fails.
var ErrVersionMismatch = errors.New("directory: the directory changed since it was read")

// Match is an If-Match precondition (ADMIN-API.md §4.5): the write
// proceeds only if the directory's version is what the caller last read.
// Checked under the write lock, so two writers cannot both pass.
type Match struct {
	Version int64
}

func checkAll(pre []Match, st *state) error {
	for _, m := range pre {
		if m.Version != st.Version {
			return ErrVersionMismatch
		}
	}
	return nil
}

// state is one device's directory: the on-disk shape of its file (and of
// the pre-item-7 global directory.json, which Migrate reads).
type state struct {
	// Schema is the file's shape (ADMIN-API.md §6.2); absent in files
	// written before 9b, which read as schema 1 and are rewritten with it.
	Schema   int                 `json:"schema,omitempty"`
	Version  int64               `json:"version"`
	Contacts map[string]*Contact `json:"contacts"`
}

// schemaVersion is what this binary writes and the highest it reads.
const schemaVersion = 1

// clone is a copy safe to keep while the original is mutated.
func (st *state) clone() *state {
	c := &state{Schema: st.Schema, Version: st.Version, Contacts: make(map[string]*Contact, len(st.Contacts))}
	for id, ct := range st.Contacts {
		cp := *ct
		c.Contacts[id] = &cp
	}
	return c
}

// Store holds every device's directory. Safe for concurrent use. Writes
// follow ADMIN-API.md §4.7: memory is mutated under mu, which is then
// released before the file is written; a failed write restores memory.
type Store struct {
	mu       sync.RWMutex
	wmu      sync.Mutex // serialises writers end to end
	dir      string     // "" → memory only
	devices  map[string]*state
	now      func() time.Time
	onChange func(deviceID string, version int64)
}

// Open loads every directory under dir (memory-only if ""), creating the
// directory on first save. Files are <device id>.json.
func Open(dir string) (*Store, error) {
	s := &Store{dir: dir, devices: map[string]*state{}, now: time.Now}
	if dir == "" {
		return s, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		st, err := readState(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("directory %s: %w", name, err)
		}
		s.devices[strings.TrimSuffix(name, ".json")] = st
	}
	return s, nil
}

func readState(path string) (*state, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st := &state{Contacts: map[string]*Contact{}}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, err
		}
		if st.Schema > schemaVersion {
			return nil, fmt.Errorf("schema %d is newer than this binary's %d; upgrade the server rather than let it misread the file", st.Schema, schemaVersion)
		}
		if st.Contacts == nil {
			st.Contacts = map[string]*Contact{}
		}
	}
	st.Schema = schemaVersion
	return st, nil
}

// Migrate moves a pre-item-7 global directory (legacyPath) into the
// directory of every device in deviceIDs: each live contact is upserted by
// URI, so running it against directories that already hold those contacts
// changes nothing. The legacy file is then renamed to legacyPath +
// ".migrated" so this runs once. With no legacy file, or no devices to
// receive it, nothing happens and the file (if any) is left where it is.
// Returns how many contacts were migrated into how many devices.
func (s *Store) Migrate(legacyPath string, deviceIDs []string) (contacts, devices int, err error) {
	if legacyPath == "" || len(deviceIDs) == 0 {
		return 0, 0, nil
	}
	if _, err := os.Stat(legacyPath); errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	} else if err != nil {
		return 0, 0, err
	}
	legacy, err := readState(legacyPath)
	if err != nil {
		return 0, 0, fmt.Errorf("legacy directory: %w", err)
	}
	var live []Contact
	for _, c := range legacy.Contacts {
		if !c.Deleted {
			live = append(live, *c)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Version < live[j].Version })
	for _, id := range deviceIDs {
		for _, c := range live {
			c.ID = "" // by URI into this device's list
			// A star the device already gave this number is the user's; the
			// global list never had one, so keep it.
			c.Favourite = s.favourite(id, c.URI)
			if _, err := s.Upsert(id, c); err != nil {
				return 0, 0, fmt.Errorf("migrating into %s: %w", id, err)
			}
		}
	}
	if err := os.Rename(legacyPath, legacyPath+".migrated"); err != nil {
		return 0, 0, err
	}
	return len(live), len(deviceIDs), nil
}

// favourite reports whether deviceID's live contact for uri, if any, is
// starred.
func (s *Store) favourite(deviceID, uri string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.devices[deviceID]
	if !ok {
		return false
	}
	c := s.liveByURILocked(st, uri)
	return c != nil && c.Favourite
}

// OnChange registers a callback invoked (outside the lock) after each
// change, with the device whose directory changed and its new version.
func (s *Store) OnChange(fn func(deviceID string, version int64)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Version is the current version of deviceID's directory (0 if it has none).
func (s *Store) Version(deviceID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st, ok := s.devices[deviceID]; ok {
		return st.Version
	}
	return 0
}

// Devices lists the device ids that have a directory, sorted.
func (s *Store) Devices() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.devices))
	for id := range s.devices {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// stateLocked returns deviceID's directory, creating it if needed. Caller
// holds the write lock.
func (s *Store) stateLocked(deviceID string) *state {
	st, ok := s.devices[deviceID]
	if !ok {
		st = &state{Contacts: map[string]*Contact{}}
		s.devices[deviceID] = st
	}
	return st
}

func validate(c Contact) error {
	if c.URI == "" || (c.Mode != ModeLocal && c.Mode != ModeTrunk) {
		return ErrInvalid
	}
	return nil
}

// upsertLocked writes c into st at version v (bumping v first when it is
// zero: callers that batch several changes into one version pass it in).
// With an empty ID, a live contact with the same URI is updated in place
// and any OTHER live contacts sharing that URI are tombstoned (a URI
// identifies one endpoint, so re-seeding a directory must not accumulate
// duplicates, and this heals a directory that already accumulated them);
// otherwise an ID is assigned. Returns the stored contact and whether an
// existing live contact was updated rather than a new one added.
func (s *Store) upsertLocked(st *state, c Contact, v int64) (Contact, bool) {
	existed := false
	if c.ID == "" {
		for _, existing := range st.Contacts {
			if existing.Deleted || !strings.EqualFold(existing.URI, c.URI) {
				continue
			}
			if c.ID == "" {
				c.ID = existing.ID
				existed = true
				continue
			}
			if v == 0 {
				st.Version++
				v = st.Version
			}
			existing.Deleted = true
			existing.Version = v
			existing.UpdatedAt = s.now()
		}
	} else if old, ok := st.Contacts[c.ID]; ok && !old.Deleted {
		existed = true
	}
	if c.ID == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		c.ID = "ct_" + hex.EncodeToString(b[:])
	}
	if v == 0 {
		st.Version++
		v = st.Version
	}
	c.Version = v
	c.UpdatedAt = s.now()
	c.Deleted = false
	cp := c
	st.Contacts[c.ID] = &cp
	return c, existed
}

// Upsert creates or replaces a contact in deviceID's directory (see
// upsertLocked for the by-URI rule).
func (s *Store) Upsert(deviceID string, c Contact, pre ...Match) (Contact, error) {
	if deviceID == "" {
		return Contact{}, ErrNoDevice
	}
	if err := validate(c); err != nil {
		return Contact{}, err
	}
	if err := checkDeviceID(deviceID); err != nil {
		return Contact{}, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	backup := s.backupLocked(deviceID)
	st := s.stateLocked(deviceID)
	if err := checkAll(pre, st); err != nil {
		s.mu.Unlock()
		return Contact{}, err
	}
	out, _ := s.upsertLocked(st, c, 0)
	raw, err := marshal(st)
	v, fn := st.Version, s.onChange
	s.mu.Unlock()
	if err := s.write(deviceID, raw, err, backup); err != nil {
		return Contact{}, err
	}
	if fn != nil {
		fn(deviceID, v)
	}
	return out, nil
}

// Delete tombstones a contact in deviceID's directory. Returns false if
// unknown or already deleted.
func (s *Store) Delete(deviceID, id string, pre ...Match) (bool, error) {
	if deviceID == "" {
		return false, ErrNoDevice
	}
	if err := checkDeviceID(deviceID); err != nil {
		return false, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	st, ok := s.devices[deviceID]
	if !ok {
		s.mu.Unlock()
		return false, nil
	}
	if err := checkAll(pre, st); err != nil {
		s.mu.Unlock()
		return false, err
	}
	c, ok := st.Contacts[id]
	if !ok || c.Deleted {
		s.mu.Unlock()
		return false, nil
	}
	backup := st.clone()
	st.Version++
	c.Deleted = true
	c.Version = st.Version
	c.UpdatedAt = s.now()
	raw, err := marshal(st)
	v, fn := st.Version, s.onChange
	s.mu.Unlock()
	if err := s.write(deviceID, raw, err, backup); err != nil {
		return false, err
	}
	if fn != nil {
		fn(deviceID, v)
	}
	return true, nil
}

// ReplaceResult says what Replace did.
type ReplaceResult struct {
	Version int64 `json:"version"`
	Added   int   `json:"added"`
	Changed int   `json:"changed"`
	Removed int   `json:"removed"`
}

// Replace makes contacts the whole of deviceID's directory (the CSV upload,
// SPEC §4.8): each is upserted by URI, every live contact whose URI is not
// among them is tombstoned, and the directory's version is bumped once for
// the lot, so a client syncs the whole change as one delta. A contact that
// is already present and identical is left alone (and not counted).
func (s *Store) Replace(deviceID string, contacts []Contact, pre ...Match) (ReplaceResult, error) {
	if deviceID == "" {
		return ReplaceResult{}, ErrNoDevice
	}
	for _, c := range contacts {
		if err := validate(c); err != nil {
			return ReplaceResult{}, err
		}
	}
	if err := checkDeviceID(deviceID); err != nil {
		return ReplaceResult{}, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	backup := s.backupLocked(deviceID)
	st := s.stateLocked(deviceID)
	if err := checkAll(pre, st); err != nil {
		if backup == nil {
			delete(s.devices, deviceID)
		}
		s.mu.Unlock()
		return ReplaceResult{}, err
	}
	res, changed := s.replaceLocked(st, contacts)
	var raw []byte
	var err error
	if changed {
		raw, err = marshal(st)
	}
	fn := s.onChange
	s.mu.Unlock()
	if changed {
		if err := s.write(deviceID, raw, err, backup); err != nil {
			return ReplaceResult{}, err
		}
	}
	if changed && fn != nil {
		fn(deviceID, res.Version)
	}
	return res, nil
}

// DryRun is Replace's answer without Replace's effect: the counts and the
// current version, nothing written, nobody notified (ADMIN-API.md §5.5).
// It runs the same reconcile on a copy, so the numbers are exactly what
// the real thing would do.
func (s *Store) DryRun(deviceID string, contacts []Contact) (ReplaceResult, error) {
	if deviceID == "" {
		return ReplaceResult{}, ErrNoDevice
	}
	for _, c := range contacts {
		if err := validate(c); err != nil {
			return ReplaceResult{}, err
		}
	}
	s.mu.RLock()
	st, ok := s.devices[deviceID]
	var copy *state
	if ok {
		copy = st.clone()
	} else {
		copy = &state{Contacts: map[string]*Contact{}}
	}
	s.mu.RUnlock()
	current := copy.Version
	res, _ := s.replaceLocked(copy, contacts)
	res.Version = current
	return res, nil
}

// replaceLocked is the reconcile behind Replace and DryRun, applied to st
// in place; the caller owns the lock or the copy. Returns what it did and
// whether anything changed (the version is bumped only then).
func (s *Store) replaceLocked(st *state, contacts []Contact) (ReplaceResult, bool) {
	var res ReplaceResult
	v := st.Version + 1
	keep := map[string]bool{}
	for _, c := range contacts {
		c.ID = ""
		keep[strings.ToLower(c.URI)] = true
		if same := s.liveByURILocked(st, c.URI); same != nil &&
			same.DisplayName == c.DisplayName && same.Mode == c.Mode && same.Favourite == c.Favourite {
			continue
		}
		_, existed := s.upsertLocked(st, c, v)
		if existed {
			res.Changed++
		} else {
			res.Added++
		}
	}
	for _, existing := range st.Contacts {
		if existing.Deleted || keep[strings.ToLower(existing.URI)] {
			continue
		}
		existing.Deleted = true
		existing.Version = v
		existing.UpdatedAt = s.now()
		res.Removed++
	}
	changed := res.Added+res.Changed+res.Removed > 0
	if changed {
		st.Version = v
	}
	res.Version = st.Version
	return res, changed
}

// liveByURILocked finds the one live contact with uri, if any. When
// duplicates exist (pre-dedupe accumulation) it returns nil so the caller
// upserts and collapses them.
func (s *Store) liveByURILocked(st *state, uri string) *Contact {
	var found *Contact
	for _, c := range st.Contacts {
		if c.Deleted || !strings.EqualFold(c.URI, uri) {
			continue
		}
		if found != nil {
			return nil
		}
		found = c
	}
	return found
}

// Purge deletes deviceID's directory and its file outright (an admin
// removing a device for good). Nothing is notified: the device is gone.
func (s *Store) Purge(deviceID string) error {
	if err := checkDeviceID(deviceID); err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	// The file first, with no lock held: if it cannot go, nothing changes.
	if s.dir != "" {
		if err := os.Remove(s.path(deviceID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.mu.Lock()
	delete(s.devices, deviceID)
	s.mu.Unlock()
	return nil
}

// Changes returns every contact in deviceID's directory changed after
// since, ordered by version, plus the current version. since == 0 is a
// full sync and omits tombstones.
func (s *Store) Changes(deviceID string, since int64) ([]Contact, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Contact{}
	st, ok := s.devices[deviceID]
	if !ok {
		return out, 0
	}
	for _, c := range st.Contacts {
		if c.Version <= since {
			continue
		}
		if since == 0 && c.Deleted {
			continue
		}
		out = append(out, *c)
	}
	sortContacts(out)
	return out, st.Version
}

// Contacts returns deviceID's live contacts, ordered by version, and the
// current version.
func (s *Store) Contacts(deviceID string) ([]Contact, int64) {
	return s.Changes(deviceID, 0)
}

func sortContacts(out []Contact) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].ID < out[j].ID
	})
}

func (s *Store) path(deviceID string) string {
	return filepath.Join(s.dir, deviceID+".json")
}

// checkDeviceID refuses an id that could name a path outside dir. Checked
// before any mutation, so a bad id changes nothing.
func checkDeviceID(deviceID string) error {
	if strings.ContainsAny(deviceID, `/\`) || deviceID == "." || deviceID == ".." {
		return fmt.Errorf("directory: bad device id %q", deviceID)
	}
	return nil
}

// backupLocked is a copy of deviceID's directory to restore if a write
// fails, or nil when it has none yet. Caller holds the write lock.
func (s *Store) backupLocked(deviceID string) *state {
	if st, ok := s.devices[deviceID]; ok {
		return st.clone()
	}
	return nil
}

// marshal serialises one directory for its file; CPU only, done under the
// lock so the bytes match memory, never I/O.
func marshal(st *state) ([]byte, error) {
	st.Schema = schemaVersion
	return json.MarshalIndent(st, "", "  ")
}

// write persists one device's marshalled directory atomically with no
// lock held (temp file, then rename: a crash mid-write leaves every other
// device's file untouched, SPEC §4.8). marshalErr is the marshal's result,
// folded in so callers have one failure path. On any failure memory is
// restored to backup (nil: the directory did not exist), so the entry is
// unchanged (ADMIN-API.md §4.4).
func (s *Store) write(deviceID string, raw []byte, marshalErr error, backup *state) error {
	err := marshalErr
	if err == nil && s.dir != "" {
		err = s.writeFile(deviceID, raw)
	}
	if err == nil {
		return nil
	}
	s.mu.Lock()
	if backup == nil {
		delete(s.devices, deviceID)
	} else {
		s.devices[deviceID] = backup
	}
	s.mu.Unlock()
	return err
}

func (s *Store) writeFile(deviceID string, raw []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := s.path(deviceID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
