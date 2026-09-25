package enroll

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"dialler/server/internal/admin"
	"dialler/server/internal/events"
)

// Body limits per route (ADMIN-API.md §5).
const (
	deviceBodyLimit = 4 << 10
	configBodyLimit = 16 << 10
	lineBodyLimit   = 4 << 10
)

// Field bounds (ADMIN-API.md §5.1, §5.3, §5.4).
const (
	maxDescription = 120
	maxSSID        = 32
	maxSSIDs       = 32
	maxLineField   = 128
	maxDN          = 32
	minFixedToken  = 16
)

var dnRe = regexp.MustCompile(`^[0-9+*#]{1,32}$`)

// NewAdminHandler serves the admin enrolment API (ADMIN-API.md §5.1–§5.4),
// behind the listener's bearer guard and again behind its own, since a
// handler mounted without the front door must still refuse strangers:
//
//	POST   /v1/admin/devices                  {"user"[,"device_id","label","token"]}
//	                                          → 201 {"device_id","user","label","code","expires_at","url"[,"token"]}
//	GET    /v1/admin/devices                  → [Device]
//	DELETE /v1/admin/devices/{id}             → 204 (revoke)
//	POST   /v1/admin/devices/{id}/enrol-code  → {"code","expires_at","url"}
//	GET    /v1/admin/devices/{id}/config      → {"version","ssids"} (404 until set)
//	PUT    /v1/admin/devices/{id}/config      {"ssids":[…]} → {"version","ssids"}   (POST accepted too)
//	GET    /v1/admin/devices/{id}/pbx-line    → {"dn","digest_user","configured"}  (404 until set)
//	PUT    /v1/admin/devices/{id}/pbx-line    {"digest_user","secret"[,"dn"]} → the same view (POST too)
//	DELETE /v1/admin/devices/{id}/pbx-line    → 204
//
// Every body is decoded strictly (admin.Decode) and validated before the
// store is touched; every error is the §4.4 envelope. The PBX line's
// secret is write-only: it goes in, and nothing — no read, no device, no
// log — gets it back out (SPEC §6 item 3c).
//
// Adding a device mints its enrolment code; a "token" in the request (the
// harness's fixed fixtures) also issues that credential at once.
func NewAdminHandler(store *Store, adminToken string, link Link, hooks Hooks) http.Handler {
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || !admin.BearerMatches(r, adminToken) {
				admin.WriteError(w, http.StatusUnauthorized, admin.CodeUnauthorized, "unauthorized")
				return
			}
			h(w, r)
		}
	}
	// deviceID checks the path's {id} against its grammar (§4.3) before any
	// store call; one that cannot name a device is 404.
	deviceID := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		id := r.PathValue("id")
		if !admin.DeviceIDRe.MatchString(id) {
			admin.NotFound(w, "device")
			return "", false
		}
		return id, true
	}

	mux.HandleFunc("POST /v1/admin/devices", guard(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			DeviceID    string `json:"device_id"`
			User        string `json:"user"`
			Description string `json:"description"`
			// Optional fixed token (dev fixtures); no credential is issued
			// when absent — the device claims its code for one.
			Token string `json:"token"`
		}
		if !admin.Decode(w, r, deviceBodyLimit, &in) {
			return
		}
		switch {
		case in.User == "":
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "user is required", "user")
			return
		case !admin.UserRe.MatchString(in.User):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, "user must be 1–64 characters of letters, digits, . _ + -", "user")
			return
		case in.DeviceID != "" && !admin.DeviceIDRe.MatchString(in.DeviceID):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, "device_id must be 1–64 characters of letters, digits, _ -", "device_id")
			return
		case !admin.Clean(in.Description, maxDescription):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("description must be at most %d characters with no control characters", maxDescription), "description")
			return
		case in.Token != "" && (len(in.Token) < minFixedToken || !admin.Clean(in.Token, 512)):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("token must be at least %d characters", minFixedToken), "token")
			return
		}
		// ADMIN-API.md §5.1: an existing id without a token is a mistake
		// (409); with a token it is the harness re-provisioning, which
		// replaces the credential and keeps everything else.
		id := in.DeviceID
		exists := id != "" && store.Exists(id)
		if exists && in.Token == "" {
			admin.WriteFieldError(w, http.StatusConflict, admin.CodeDeviceExists, id+" already exists; mint it a code or purge it first", "device_id")
			return
		}
		description := in.Description
		if !exists {
			var err error
			id, err = store.Create(id, in.User, in.Description)
			if err != nil {
				storeError(w, err)
				return
			}
		} else if d, ok := store.Device(id); ok {
			description = d.Description
		}
		out := map[string]any{"device_id": id, "user": in.User, "description": description}
		if in.Token != "" {
			tok, err := store.IssueToken(id, in.User, in.Token)
			if err != nil {
				storeError(w, err)
				return
			}
			out["token"] = tok
		}
		code, expires, err := store.MintCode(id)
		if err != nil {
			storeError(w, err)
			return
		}
		out["code"], out["expires_at"], out["url"] = code, expires, link.URL(code)
		if !exists {
			hooks.event(events.KindDeviceCreated, id, map[string]any{"user": in.User})
		}
		hooks.event(events.KindCodeIssued, id, map[string]any{"expires_at": expires})
		if hooks.OnIssue != nil {
			hooks.OnIssue(id, in.User)
		}
		admin.WriteJSON(w, http.StatusCreated, out)
	}))

	mux.HandleFunc("GET /v1/admin/devices", guard(func(w http.ResponseWriter, r *http.Request) {
		admin.WriteJSON(w, http.StatusOK, store.Devices())
	}))

	mux.HandleFunc("DELETE /v1/admin/devices/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		// ?purge=1 deletes the record outright (§5.1); a plain DELETE is
		// the revoke it has always been. Any other value is a mistake.
		purge := false
		switch r.URL.Query().Get("purge") {
		case "":
		case "1", "true":
			purge = true
		default:
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, "purge must be 1 when given", "purge")
			return
		}
		var (
			found bool
			err   error
		)
		if purge {
			found, err = store.Purge(id)
		} else {
			found, err = store.Revoke(id)
		}
		if err != nil {
			storeError(w, err)
			return
		}
		if !found {
			admin.NotFound(w, "device")
			return
		}
		switch {
		case purge && hooks.OnPurge != nil:
			hooks.OnPurge(id)
		case !purge && hooks.OnRevoke != nil:
			hooks.OnRevoke(id)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("GET /v1/admin/devices/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		d, ok := store.Device(id)
		if !ok {
			admin.NotFound(w, "device")
			return
		}
		admin.WriteJSON(w, http.StatusOK, d)
	}))

	mux.HandleFunc("PATCH /v1/admin/devices/{id}", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		var in struct {
			Description *string `json:"description"`
		}
		if !admin.Decode(w, r, deviceBodyLimit, &in) {
			return
		}
		if in.Description == nil {
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "description is required (it may be empty)", "description")
			return
		}
		if !admin.Clean(*in.Description, maxDescription) {
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("description must be at most %d characters with no control characters", maxDescription), "description")
			return
		}
		pre, done := recordMatch(w, r)
		if done {
			return
		}
		found, err := store.SetDescription(id, *in.Description, pre...)
		if err != nil {
			storeError(w, err)
			return
		}
		if !found {
			admin.NotFound(w, "device")
			return
		}
		hooks.event(events.KindDescriptionChanged, id, nil)
		d, _ := store.Device(id)
		admin.WriteJSON(w, http.StatusOK, d)
	}))

	mux.HandleFunc("DELETE /v1/admin/devices/{id}/enrol-code", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		found, err := store.CancelCode(id)
		if err != nil {
			storeError(w, err)
			return
		}
		if !found {
			admin.NotFound(w, "device")
			return
		}
		hooks.event(events.KindCodeCancelled, id, nil)
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("POST /v1/admin/devices/{id}/enrol-code", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		code, expires, err := store.MintCode(id)
		if errors.Is(err, ErrInvalid) {
			admin.NotFound(w, "device")
			return
		}
		if err != nil {
			storeError(w, err)
			return
		}
		hooks.event(events.KindCodeIssued, id, map[string]any{"expires_at": expires})
		admin.WriteJSON(w, http.StatusOK, codeResponse{Code: code, ExpiresAt: expires, URL: link.URL(code)})
	}))

	mux.HandleFunc("GET /v1/admin/devices/{id}/config", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		if !store.Exists(id) {
			admin.NotFound(w, "device")
			return
		}
		cfg := store.Config(id)
		if cfg == nil {
			admin.NotFound(w, "settings for this device")
			return
		}
		admin.WriteJSON(w, http.StatusOK, cfg)
	}))
	setConfig := guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		var in struct {
			SSIDs []string `json:"ssids"`
		}
		if !admin.Decode(w, r, configBodyLimit, &in) {
			return
		}
		if in.SSIDs == nil {
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "ssids is required (an empty list removes Local Push on the phone)", "ssids")
			return
		}
		if len(in.SSIDs) > maxSSIDs {
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("at most %d ssids", maxSSIDs), "ssids")
			return
		}
		for i, s := range in.SSIDs {
			if !admin.Clean(s, maxSSID) {
				admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("an SSID is at most %d bytes with no control characters", maxSSID), fmt.Sprintf("ssids[%d]", i))
				return
			}
		}
		// If-Match here is the settings' version (§4.5).
		var pre []Match
		if v, sent, done := admin.IfMatchInt(w, r); done {
			return
		} else if sent {
			pre = append(pre, Match{ConfigVersion: v})
		}
		cfg, changed, err := store.SetConfig(id, in.SSIDs, pre...)
		if errors.Is(err, ErrInvalid) {
			admin.NotFound(w, "device")
			return
		}
		if err != nil {
			storeError(w, err)
			return
		}
		// An identical list is a no-op: nothing is pushed (§5.3).
		if changed && hooks.OnConfig != nil {
			hooks.OnConfig(id, cfg)
		}
		admin.WriteJSON(w, http.StatusOK, cfg)
	})
	mux.HandleFunc("PUT /v1/admin/devices/{id}/config", setConfig)
	// busybox wget (the harness's in-network helper) has no PUT.
	mux.HandleFunc("POST /v1/admin/devices/{id}/config", setConfig)

	mux.HandleFunc("GET /v1/admin/devices/{id}/pbx-line", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		if !store.Exists(id) {
			admin.NotFound(w, "device")
			return
		}
		line, ok := store.PBXLine(id)
		if !ok {
			admin.NotFound(w, "PBX line for this device")
			return
		}
		// Deliberately the view type: there is no query parameter, no
		// header and no debug mode that returns the secret.
		admin.WriteJSON(w, http.StatusOK, line)
	}))
	setLine := guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		var in struct {
			DN         string `json:"dn"`
			DigestUser string `json:"digest_user"`
			Secret     string `json:"secret"`
		}
		if !admin.Decode(w, r, lineBodyLimit, &in) {
			return
		}
		switch {
		case in.DigestUser == "":
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "digest_user is required", "digest_user")
			return
		case in.Secret == "":
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeMissing, "secret is required", "secret")
			return
		case !admin.Clean(in.DigestUser, maxLineField):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("digest_user is at most %d bytes with no control characters", maxLineField), "digest_user")
			return
		case !admin.Clean(in.Secret, maxLineField):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("secret is at most %d bytes with no control characters", maxLineField), "secret")
			return
		case in.DN != "" && !dnRe.MatchString(in.DN):
			admin.WriteFieldError(w, http.StatusBadRequest, admin.CodeInvalid, fmt.Sprintf("dn is at most %d of digits, + * #", maxDN), "dn")
			return
		}
		if !store.Exists(id) {
			admin.NotFound(w, "device")
			return
		}
		pre, done := recordMatch(w, r)
		if done {
			return
		}
		line, err := store.SetPBXLine(id, in.DN, in.DigestUser, in.Secret, pre...)
		switch {
		case errors.Is(err, ErrInvalid):
			admin.NotFound(w, "device")
			return
		case errors.Is(err, ErrNoSecretKey):
			admin.WriteError(w, http.StatusServiceUnavailable, admin.CodeUnavailable, err.Error())
			return
		case err != nil:
			storeError(w, err)
			return
		}
		if hooks.OnPBXLine != nil {
			cred, ok, err := store.PBXCredential(id)
			if err != nil || !ok {
				admin.WriteError(w, http.StatusInternalServerError, admin.CodeStore, "the line was saved but cannot be read back")
				return
			}
			hooks.OnPBXLine(id, &cred)
		}
		admin.WriteJSON(w, http.StatusOK, line)
	})
	mux.HandleFunc("PUT /v1/admin/devices/{id}/pbx-line", setLine)
	mux.HandleFunc("POST /v1/admin/devices/{id}/pbx-line", setLine)

	mux.HandleFunc("DELETE /v1/admin/devices/{id}/pbx-line", guard(func(w http.ResponseWriter, r *http.Request) {
		id, ok := deviceID(w, r)
		if !ok {
			return
		}
		pre, done := recordMatch(w, r)
		if done {
			return
		}
		ok, err := store.DeletePBXLine(id, pre...)
		if err != nil {
			storeError(w, err)
			return
		}
		if !ok {
			admin.NotFound(w, "device")
			return
		}
		if hooks.OnPBXLine != nil {
			hooks.OnPBXLine(id, nil)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// storeError maps a store failure: ErrInvalid is the store refusing
// arguments the handler should already have validated (a 400 rather than
// a 500, so the fuzzer's finding is visible as such), anything else is
// the disk.
// recordMatch reads an If-Match carrying the record's updated_at (§4.5).
// done means a malformed header has been answered.
func recordMatch(w http.ResponseWriter, r *http.Request) (pre []Match, done bool) {
	t, sent, done := admin.IfMatchTime(w, r)
	if done {
		return nil, true
	}
	if sent {
		pre = append(pre, Match{UpdatedAt: t})
	}
	return pre, false
}

func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrVersionMismatch):
		admin.PreconditionFailed(w)
	case errors.Is(err, ErrInvalid):
		admin.WriteError(w, http.StatusBadRequest, admin.CodeInvalid, err.Error())
	case errors.Is(err, ErrExists):
		admin.WriteFieldError(w, http.StatusConflict, admin.CodeDeviceExists, err.Error(), "device_id")
	case errors.Is(err, ErrUserTaken):
		admin.WriteFieldError(w, http.StatusConflict, admin.CodeUserTaken, err.Error(), "user")
	case errors.Is(err, ErrImmutable):
		admin.WriteFieldError(w, http.StatusConflict, admin.CodeImmutable, err.Error(), "user")
	default:
		admin.StoreError(w, err)
	}
}
