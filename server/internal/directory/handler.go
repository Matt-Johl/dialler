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

// NewHandler serves the directory API.
//
//	GET    /v1/directory?since=N       device auth → SyncResponse
//	PUT    /v1/directory/{id}          admin       → Contact
//	POST   /v1/directory               admin       → Contact (id assigned)
//	DELETE /v1/directory/{id}          admin       → 204
func NewHandler(store *Store, deviceAuth func(http.Handler) http.Handler, adminToken string) http.Handler {
	mux := http.NewServeMux()
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || !enroll.BearerMatches(r, adminToken) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}

	mux.Handle("GET /v1/directory", deviceAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		if since < 0 {
			since = 0
		}
		contacts, version := store.Changes(since)
		writeJSON(w, http.StatusOK, SyncResponse{Version: version, Since: since, Contacts: contacts})
	})))

	upsert := func(w http.ResponseWriter, r *http.Request, id string) {
		var c Contact
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&c); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if id != "" {
			c.ID = id
		}
		out, err := store.Upsert(c)
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
	mux.HandleFunc("POST /v1/directory", admin(func(w http.ResponseWriter, r *http.Request) { upsert(w, r, "") }))
	mux.HandleFunc("PUT /v1/directory/{id}", admin(func(w http.ResponseWriter, r *http.Request) { upsert(w, r, r.PathValue("id")) }))
	mux.HandleFunc("DELETE /v1/directory/{id}", admin(func(w http.ResponseWriter, r *http.Request) {
		ok, err := store.Delete(r.PathValue("id"))
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

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
