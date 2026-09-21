package directory

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"dialler/server/internal/enroll"
)

// SyncResponse is the body of GET /v1/directory.
type SyncResponse struct {
	Version  int64     `json:"version"`
	Since    int64     `json:"since"`
	Contacts []Contact `json:"contacts"`
}

// ListResponse is the body of GET /v1/admin/devices/{id}/directory and the
// request body of PUT on the same path.
type ListResponse struct {
	Version  int64     `json:"version"`
	Contacts []Contact `json:"contacts"`
}

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
// the by-URI collapse of duplicates reaches it as tombstones.
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
		upsert(store, w, r, deviceID, "")
	}))
	mux.Handle("PUT /v1/directory/{id}", device(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		upsert(store, w, r, deviceID, r.PathValue("id"))
	}))
	mux.Handle("DELETE /v1/directory/{id}", device(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		remove(store, w, r, deviceID, r.PathValue("id"))
	}))
	return mux
}

// NewAdminHandler serves the operator's view of every device's directory,
// guarded by the static admin bearer token (SPEC §4.8). knownDevice says
// whether a device id is enrolled; writes to an unknown one are 404, so a
// typo cannot create a directory nobody will ever read.
//
//	GET    /v1/admin/devices/{id}/directory         → ListResponse (live contacts)
//	PUT    /v1/admin/devices/{id}/directory         {contacts:[…]} → ReplaceResult (replace-all)
//	POST   /v1/admin/devices/{id}/directory         → Contact
//	PUT    /v1/admin/devices/{id}/directory/{cid}   → Contact
//	DELETE /v1/admin/devices/{id}/directory/{cid}   → 204
func NewAdminHandler(store *Store, adminToken string, knownDevice func(deviceID string) bool) http.Handler {
	mux := http.NewServeMux()
	admin := func(h func(w http.ResponseWriter, r *http.Request, deviceID string)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || !enroll.BearerMatches(r, adminToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			id := r.PathValue("id")
			if knownDevice != nil && !knownDevice(id) {
				http.Error(w, "unknown device", http.StatusNotFound)
				return
			}
			h(w, r, id)
		}
	}

	mux.HandleFunc("GET /v1/admin/devices/{id}/directory", admin(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		contacts, version := store.Contacts(deviceID)
		writeJSON(w, http.StatusOK, ListResponse{Version: version, Contacts: contacts})
	}))
	mux.HandleFunc("PUT /v1/admin/devices/{id}/directory", admin(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		var in ListResponse
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&in); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		res, err := store.Replace(deviceID, in.Contacts)
		if errors.Is(err, ErrInvalid) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, res)
	}))
	mux.HandleFunc("POST /v1/admin/devices/{id}/directory", admin(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		upsert(store, w, r, deviceID, "")
	}))
	mux.HandleFunc("PUT /v1/admin/devices/{id}/directory/{cid}", admin(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		upsert(store, w, r, deviceID, r.PathValue("cid"))
	}))
	mux.HandleFunc("DELETE /v1/admin/devices/{id}/directory/{cid}", admin(func(w http.ResponseWriter, r *http.Request, deviceID string) {
		remove(store, w, r, deviceID, r.PathValue("cid"))
	}))
	return mux
}

func upsert(store *Store, w http.ResponseWriter, r *http.Request, deviceID, id string) {
	var c Contact
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
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

func remove(store *Store, w http.ResponseWriter, r *http.Request, deviceID, id string) {
	ok, err := store.Delete(deviceID, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
