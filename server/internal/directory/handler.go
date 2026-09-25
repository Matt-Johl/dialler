package directory

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"dialler/server/internal/admin"
)

// SyncResponse is the body of GET /v1/directory.
type SyncResponse struct {
	Version  int64     `json:"version"`
	Since    int64     `json:"since"`
	Contacts []Contact `json:"contacts"`
}

// ListResponse is the body of GET /v1/admin/devices/{id}/directory and the
// request body of PUT on the same path (version is accepted there so a
// GET body can be sent straight back, and ignored).
type ListResponse struct {
	Version  int64     `json:"version"`
	Contacts []Contact `json:"contacts"`
}

// Body limits and bounds (ADMIN-API.md §5.5).
const (
	replaceBodyLimit = 4 << 20
	contactBodyLimit = 64 << 10
	maxContacts      = 5000
	maxDisplayName   = 120
	maxURI           = 256
)

// NewHandler serves the device-facing directory API. Every route is
// device-authenticated and scoped to the calling device's own directory
// (the X-Device-ID the middleware verified):
//
//	GET    /v1/directory?since=N   → SyncResponse
//	POST   /v1/directory           → Contact (id assigned; by URI)
//	PUT    /v1/directory/{id}      → Contact
//	DELETE /v1/directory/{id}      → 204
//
// A write answers with the contact as stored; the device then syncs from
// its cursor as usual (the server also pushes directory_changed to it), so
// the by-URI collapse of duplicates reaches it as tombstones. This API is
// unchanged by the admin contract: plain-text errors, lenient decoding.
func NewHandler(store *Store, deviceAuth func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	device := func(h func(w http.ResponseWriter, r *http.Request, deviceID string)) http.Handler {
		return deviceAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h(w, r, r.Header.Get("X-Device-ID"))
		}))
	}

	mux.Handle("GET /v1/directory", device(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if since < 0 {
			since = 0
		}
		contacts, version := store.Changes(deviceID, since)
		writeJSON(w, http.StatusOK, SyncResponse{Version: version, Since: since, Contacts: contacts})
	}))
	mux.Handle("POST /v1/directory", device(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		deviceUpsert(store, w, r, deviceID, "")
	}))
	mux.Handle("PUT /v1/directory/{id}", device(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		deviceUpsert(store, w, r, deviceID, r.PathValue("id"))
	}))
	mux.Handle("DELETE /v1/directory/{id}", device(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		ok, err := store.Delete(deviceID, r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// deviceUpsert is the device-facing write, as it has always been.
func deviceUpsert(store *Store, w http.ResponseWriter, r *http.Request, deviceID, id string) {
	var c Contact
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, contactBodyLimit)).Decode(&c); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if id != "" {
		c.ID = id
	}
	out, err := store.Upsert(deviceID, c)
	if errors.Is(err, ErrInvalid) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// NewAdminHandler serves the operator's view of every device's directory
// (ADMIN-API.md §5.5), behind the listener's bearer guard and its own.
// knownDevice says whether a device id exists; writes to an unknown one
// are 404, so a typo cannot create a directory nobody will ever read.
//
//	GET    /v1/admin/devices/{id}/directory         → ListResponse (live contacts)
//	PUT    /v1/admin/devices/{id}/directory         {contacts:[…]} → ReplaceResult (replace-all)
//	POST   /v1/admin/devices/{id}/directory         → Contact
//	PUT    /v1/admin/devices/{id}/directory/{cid}   → Contact
//	DELETE /v1/admin/devices/{id}/directory/{cid}   → 204
//
// Bodies are decoded strictly and validated in full before the store is
// touched; errors are the §4.4 envelope naming the field.
func NewAdminHandler(store *Store, adminToken string, knownDevice func(deviceID string) bool) http.Handler {
	mux := http.NewServeMux()
	guarded := func(h func(w http.ResponseWriter, r *http.Request, deviceID string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || !admin.BearerMatches(r, adminToken) {
				admin.WriteError(w, http.StatusUnauthorized, admin.CodeUnauthorized, "unauthorized")
				return
			}
			id := r.PathValue("id")
			if !admin.DeviceIDRe.MatchString(id) || (knownDevice != nil && !knownDevice(id)) {
				admin.NotFound(w, "device")
				return
			}
			h(w, r, id)
		}
	}
	// contactID checks {cid} against its grammar before the store is asked.
	contactID := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		cid := r.PathValue("cid")
		if !admin.ContactIDRe.MatchString(cid) {
			admin.NotFound(w, "contact")
			return "", false
		}
		return cid, true
	}

	mux.HandleFunc("GET /v1/admin/devices/{id}/directory", guarded(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		contacts, version := store.Contacts(deviceID)
		admin.WriteJSON(w, http.StatusOK, ListResponse{Version: version, Contacts: contacts})
	}))
	mux.HandleFunc("PUT /v1/admin/devices/{id}/directory", guarded(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		var in ListResponse
		if !admin.Decode(w, r, replaceBodyLimit, &in) {
			return
		}
		if in.Contacts == nil {
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "contacts is required (an empty list removes every contact)", "contacts")
			return
		}
		if len(in.Contacts) > maxContacts {
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("at most %d contacts", maxContacts), "contacts")
			return
		}
		seen := make(map[string]int, len(in.Contacts))
		for i, c := range in.Contacts {
			if !validateContact(w, c, fmt.Sprintf("contacts[%d]", i)) {
				return
			}
			key := strings.ToLower(c.URI)
			if j, dup := seen[key]; dup {
				admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("duplicate uri, same as contacts[%d]", j), fmt.Sprintf("contacts[%d].uri", i))
				return
			}
			seen[key] = i
		}
		// ?dry_run=1: the counts and the current version, nothing written,
		// nothing pushed (§5.5) — what a client shows before an upload.
		dryRun := false
		switch r.URL.Query().Get("dry_run") {
		case "":
		case "1", "true":
			dryRun = true
		default:
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, "dry_run must be 1 when given", "dry_run")
			return
		}
		var res ReplaceResult
		var err error
		if dryRun {
			res, err = store.DryRun(deviceID, in.Contacts)
		} else {
			pre, done := versionMatch(w, r)
			if done {
				return
			}
			res, err = store.Replace(deviceID, in.Contacts, pre...)
		}
		if err != nil {
			storeError(w, err)
			return
		}
		admin.WriteJSON(w, http.StatusOK, res)
	}))
	mux.HandleFunc("POST /v1/admin/devices/{id}/directory", guarded(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		adminUpsert(store, w, r, deviceID, "")
	}))
	mux.HandleFunc("PUT /v1/admin/devices/{id}/directory/{cid}", guarded(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		cid, ok := contactID(w, r)
		if !ok {
			return
		}
		adminUpsert(store, w, r, deviceID, cid)
	}))
	mux.HandleFunc("DELETE /v1/admin/devices/{id}/directory/{cid}", guarded(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		cid, ok := contactID(w, r)
		if !ok {
			return
		}
		pre, done := versionMatch(w, r)
		if done {
			return
		}
		ok, err := store.Delete(deviceID, cid, pre...)
		if err != nil {
			storeError(w, err)
			return
		}
		if !ok {
			admin.NotFound(w, "contact")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// validateContact is the §5.5 field check; on a fault it has written the
// error, with prefix naming the contact in a list, and returns false.
func validateContact(w http.ResponseWriter, c Contact, prefix string) bool {
	field := func(name string) string {
		if prefix == "" {
			return name
		}
		return prefix + "." + name
	}
	switch {
	case c.URI == "":
		admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "uri is required", field("uri"))
	case !admin.Clean(c.URI, maxURI) || strings.ContainsAny(c.URI, " \t"):
		admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("uri is at most %d bytes with no spaces or control characters", maxURI), field("uri"))
	case c.Mode != ModeLocal && c.Mode != ModeTrunk:
		admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, `mode must be "local" or "trunk"`, field("mode"))
	case !admin.Clean(c.DisplayName, maxDisplayName):
		admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("display_name is at most %d characters with no control characters", maxDisplayName), field("display_name"))
	default:
		return true
	}
	return false
}

func adminUpsert(store *Store, w http.ResponseWriter, r *http.Request, deviceID, id string) {
	var c Contact
	if !admin.Decode(w, r, contactBodyLimit, &c) {
		return
	}
	if !validateContact(w, c, "") {
		return
	}
	if id != "" {
		c.ID = id
	}
	pre, done := versionMatch(w, r)
	if done {
		return
	}
	out, err := store.Upsert(deviceID, c, pre...)
	if err != nil {
		storeError(w, err)
		return
	}
	admin.WriteJSON(w, http.StatusOK, out)
}

// versionMatch reads an If-Match carrying the directory's version (§4.5).
// done means a malformed header has been answered.
func versionMatch(w http.ResponseWriter, r *http.Request) (pre []Match, done bool) {
	v, sent, done := admin.IfMatchInt(w, r)
	if done {
		return nil, true
	}
	if sent {
		pre = append(pre, Match{Version: *v})
	}
	return pre, false
}

// storeError maps a store failure to the §4.4 envelope.
func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrVersionMismatch):
		admin.PreconditionFailed(w)
	case errors.Is(err, ErrInvalid):
		admin.WriteError(w, http.StatusBadRequest, admin.CodeInvalid, err.Error())
	default:
		admin.StoreError(w, err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
