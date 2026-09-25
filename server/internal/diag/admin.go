package diag

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"dialler/server/internal/admin"
)

// MaxList bounds one listing (ADMIN-API.md §5.11).
const MaxList = 1000

// timeLayout is the leading timestamp Store puts on every file name.
const timeLayout = "20060102T150405.000Z"

// Entry is one row of the listing.
type Entry struct {
	Name string `json:"name"`
	// Kind is the segment after the leading timestamp in the name
	// ("20260925T085512.000Z-applog-x.log" → "applog"); "" when the name
	// does not carry one.
	Kind string `json:"kind"`
	Size int64  `json:"size"`
	At   string `json:"at"` // RFC 3339 UTC mtime
}

// AdminHandler serves the admin diagnostics routes (ADMIN-API.md §5.11):
//
//	GET    /v1/admin/devices/{id}/diag         → [{"name","kind","size","at"}], newest first, at most MaxList
//	GET    /v1/admin/devices/{id}/diag/{name}  → the file, Content-Type by extension, as an attachment
//	DELETE /v1/admin/devices/{id}/diag/{name}  → 204
//	DELETE /v1/admin/devices/{id}/diag         → 204, every file of the device
//
// There is no bearer check here: the handler is mounted behind the admin
// listener's front door (admin.Chain), which refuses strangers before any
// path handling and turns the mux's own 404/405 into the §4.4 envelope.
//
// {id} must match its grammar (§4.3) and, when knownDevice is non-nil, be a
// device it knows; {name} must pass admin.ValidDiagName. Both are checked
// before the filesystem is touched, and a path is only ever built as
// filepath.Join(dir, id, name) after them, so a traversal-shaped segment
// never reaches a file path. A symlink is never followed: anything that is
// not a regular file is 404. Every error is the §4.4 envelope; a filesystem
// failure is admin.StoreError.
//
// File I/O runs on the admin goroutine, counted against the in-flight cap
// (§4.7).
func AdminHandler(dir string, knownDevice func(deviceID string) bool) http.Handler {
	mux := http.NewServeMux()

	// deviceID checks {id} against its grammar and the device set before
	// anything else; one that cannot name a device is 404.
	deviceID := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		id := r.PathValue("id")
		if !admin.DeviceIDRe.MatchString(id) || (knownDevice != nil && !knownDevice(id)) {
			admin.NotFound(w, "device")
			return "", false
		}
		return id, true
	}
	// fileName checks {name} against its grammar; one that cannot name a
	// file is 404.
	fileName := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		name := r.PathValue("name")
		if !admin.ValidDiagName(name) {
			admin.NotFound(w, "file")
			return "", false
		}
		return name, true
	}
	// regular Lstats path and returns its info when it is a regular file;
	// otherwise the answer has been written (404 for absent, a symlink, a
	// directory, anything else; 500 for a failure) and ok is false.
	regular := func(w http.ResponseWriter, path string) (fs.FileInfo, bool) {
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				admin.NotFound(w, "file")
			} else {
				admin.StoreError(w, err)
			}
			return nil, false
		}
		if !info.Mode().IsRegular() {
			admin.NotFound(w, "file")
			return nil, false
		}
		return info, true
	}

	mux.HandleFunc("GET /v1/admin/devices/{id}/diag", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		entries, err := List(dir, id)
		if err != nil {
			admin.StoreError(w, err)
			return
		}
		admin.WriteJSON(w, http.StatusOK, entries)
	})

	mux.HandleFunc("GET /v1/admin/devices/{id}/diag/{name}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		name, ok := fileName(w, r)
		if !ok {
			return
		}
		p := filepath.Join(dir, id, name)
		info, ok := regular(w, p)
		if !ok {
			return
		}
		f, err := os.Open(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				admin.NotFound(w, "file")
			} else {
				admin.StoreError(w, err)
			}
			return
		}
		defer f.Close()
		// The open must have landed on the very file that was Lstat'd: a
		// swap for a symlink between the two calls is refused too.
		opened, err := f.Stat()
		if err != nil {
			admin.StoreError(w, err)
			return
		}
		if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			admin.NotFound(w, "file")
			return
		}
		w.Header().Set("Content-Type", contentType(name))
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, name, opened.ModTime(), f)
	})

	mux.HandleFunc("DELETE /v1/admin/devices/{id}/diag/{name}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		name, ok := fileName(w, r)
		if !ok {
			return
		}
		p := filepath.Join(dir, id, name)
		if _, ok := regular(w, p); !ok {
			return
		}
		if err := os.Remove(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				admin.NotFound(w, "file")
			} else {
				admin.StoreError(w, err)
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("DELETE /v1/admin/devices/{id}/diag", func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		if err := Purge(dir, id); err != nil {
			admin.StoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

// List returns the device's uploads, newest first, at most MaxList. A device
// with no uploads has an empty (not nil) list. Only regular files are
// listed: a symlink or a directory under the device is invisible. The
// caller has validated device.
func List(dir, device string) ([]Entry, error) {
	entries := []Entry{}
	des, err := os.ReadDir(filepath.Join(dir, device))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return entries, nil
		}
		return nil, err
	}
	type row struct {
		Entry
		mtime time.Time
	}
	rows := make([]row, 0, len(des))
	for _, de := range des {
		// ReadDir does not follow symlinks, so Type is the link itself.
		if !de.Type().IsRegular() {
			continue
		}
		info, err := de.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // removed between the read and the stat
			}
			return nil, err
		}
		rows = append(rows, row{
			Entry: Entry{
				Name: de.Name(),
				Kind: Kind(de.Name()),
				Size: info.Size(),
				At:   info.ModTime().UTC().Format(time.RFC3339),
			},
			mtime: info.ModTime(),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].mtime.Equal(rows[j].mtime) {
			return rows[i].mtime.After(rows[j].mtime)
		}
		// Names lead with their upload time, so descending is newest first.
		return rows[i].Name > rows[j].Name
	})
	if len(rows) > MaxList {
		rows = rows[:MaxList]
	}
	for _, r := range rows {
		entries = append(entries, r.Entry)
	}
	return entries, nil
}

// Kind parses the kind out of a stored file name: the segment after the
// leading timestamp, up to the next "-" or the extension
// ("20260925T085512.000Z-applog-x.log" → "applog"). It is "" when the name
// does not start with a timestamp or has nothing after it.
func Kind(name string) string {
	stamp, rest, ok := strings.Cut(name, "-")
	if !ok {
		return ""
	}
	if _, err := time.Parse(timeLayout, stamp); err != nil {
		return ""
	}
	rest = strings.TrimSuffix(rest, path.Ext(rest))
	kind, _, _ := strings.Cut(rest, "-")
	return kind
}

// contentType is the download's Content-Type, by extension.
func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".log", ".txt", ".ips":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	return "application/octet-stream"
}

// yield is what Sweep calls between files; a test counts it.
var yield = runtime.Gosched

// Sweep deletes every regular file under dir/<device>/ whose mtime is
// older than now-retain and returns how many it removed. It works one
// device directory at a time and yields between files (§4.7), so the
// admin goroutine it runs on never holds the CPU for a long directory.
// A failure does not stop the walk: every error is collected and returned
// joined once the walk is done. retain <= 0 means keep forever: nothing is
// removed and Sweep returns at once. A dir that does not exist is empty.
func Sweep(dir string, retain time.Duration, now time.Time) (removed int, err error) {
	if retain <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-retain)
	devices, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var errs []error
	for _, dev := range devices {
		if !dev.IsDir() {
			continue
		}
		devDir := filepath.Join(dir, dev.Name())
		files, err := os.ReadDir(devDir)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		for _, f := range files {
			// Only a regular file is a diag upload; a symlink is left alone.
			if !f.Type().IsRegular() {
				continue
			}
			info, err := f.Info()
			if err != nil {
				if !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(devDir, f.Name())); err != nil {
					if !errors.Is(err, fs.ErrNotExist) {
						errs = append(errs, err)
					}
				} else {
					removed++
				}
			}
			yield()
		}
	}
	return removed, errors.Join(errs...)
}

// RunSweeper runs Sweep once at start and then every `every` (an hour in
// production) until ctx is done, logging "diag: swept" with the count when
// something was removed and an error when the sweep reported one. It
// blocks; the caller runs it on its own goroutine. every <= 0 runs the
// start sweep only.
func RunSweeper(ctx context.Context, dir string, retain time.Duration, every time.Duration, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	sweep := func() {
		n, err := Sweep(dir, retain, time.Now())
		if err != nil {
			log.Error("diag: sweep failed", "removed", n, "err", err)
		}
		if n > 0 {
			log.Info("diag: swept", "removed", n)
		}
	}
	sweep()
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
