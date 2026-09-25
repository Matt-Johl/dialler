package adminui

import (
	"context"
	"html/template"
	"net/http"
	"strings"
	"time"

	"dialler/server/internal/diag"
	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/loglevel"
	"dialler/server/internal/status"
)

type loglevelView struct{ loglevel.View }

// deviceData is the device page.
type deviceData struct {
	Device    enroll.Device
	Live      status.DeviceStatus
	Directory directory.ListResponse
	Diag      []diag.Entry
	Others    []enroll.Device // copy-to targets
	// A freshly minted code and its QR, shown once.
	Code   *Code
	QR     template.HTML
	Form   map[string]string
	Server status.ServerView
}

func (u *UI) device(w http.ResponseWriter, r *http.Request) {
	u.renderDevice(w, r, r.PathValue("id"), u.flashFor(r), "", nil, nil)
}

// renderDevice draws the device page; code, when set, is shown with its QR.
func (u *UI) renderDevice(w http.ResponseWriter, r *http.Request, id, flash, errMsg string, form map[string]string, code *Code) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	data, err := u.loadDevice(ctx, id)
	got, lastAt, banner := u.fetched("device:"+id, data, err)
	p := page{Title: "Device", Nav: "fleet", CSRF: csrf(r), Banner: banner, ReadOnly: banner != "", LastAt: lastAt, Flash: flash, Error: errMsg}
	if got != nil {
		d := got.(deviceData)
		d.Form = form
		if code != nil {
			d.Code = code
			if u.cfg.QR != nil {
				if svg, err := u.cfg.QR(code.URL); err == nil {
					d.QR = svg
				}
			}
		}
		p.Title = d.Device.User + " · " + d.Device.Description
		p.Data = d
	} else if err != nil && banner == "" {
		p.Error = describe(err)
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
	}
	u.render(w, "device.html", p)
}

func isNotFound(err error) bool {
	var ae *APIError
	return asAPIError(err, &ae) && ae.Status == http.StatusNotFound
}

func (u *UI) loadDevice(ctx context.Context, id string) (deviceData, error) {
	var d deviceData
	dev, err := u.cfg.Client.Device(ctx, id)
	if err != nil {
		return d, err
	}
	d.Device = dev
	st, err := u.cfg.Client.Status(ctx)
	if err != nil {
		return d, err
	}
	for _, s := range st.Devices {
		if s.DeviceID == id {
			d.Live = s
		}
	}
	if d.Live.Sessions == nil {
		d.Live.Sessions = map[string]*status.Session{}
	}
	if d.Directory, err = u.cfg.Client.Directory(ctx, id); err != nil {
		return d, err
	}
	if d.Diag, err = u.cfg.Client.Diag(ctx, id); err != nil {
		return d, err
	}
	all, err := u.cfg.Client.Devices(ctx)
	if err != nil {
		return d, err
	}
	for _, o := range all {
		if o.DeviceID != id {
			d.Others = append(d.Others, o)
		}
	}
	if d.Server, err = u.cfg.Client.Server(ctx); err != nil {
		return d, err
	}
	return d, nil
}

// ---- fleet actions ---------------------------------------------------------

func (u *UI) createDevice(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	user, desc := strings.TrimSpace(r.FormValue("user")), strings.TrimSpace(r.FormValue("description"))
	form := map[string]string{"user": user, "description": desc}
	created, err := u.cfg.Client.CreateDevice(ctx, user, desc)
	if err != nil {
		u.renderFleet(w, r, "", describe(err), form)
		return
	}
	code := &Code{Code: created.Code, ExpiresAt: created.ExpiresAt, URL: created.URL}
	u.renderDevice(w, r, created.DeviceID, "Device added. Scan the code or type it into the phone; it is valid until "+created.ExpiresAt.Local().Format("15:04")+".", "", nil, code)
}

// ---- device actions --------------------------------------------------------

func (u *UI) setDescription(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	desc := strings.TrimSpace(r.FormValue("description"))
	if _, err := u.cfg.Client.SetDescription(ctx, id, desc, r.FormValue("updated_at")); err != nil {
		u.renderDevice(w, r, id, "", describe(err), map[string]string{"description": desc}, nil)
		return
	}
	u.redirectDevice(w, r, id, "Description saved.")
}

func (u *UI) revoke(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.Revoke(ctx, id); err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	u.redirectDevice(w, r, id, "Revoked: the phone can no longer connect or register. Its directory, settings and line are kept; mint a code to re-enrol it.")
}

func (u *UI) purge(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if r.FormValue("confirm") != id {
		u.renderDevice(w, r, id, "", "To purge, type the device id into the confirmation box.", nil, nil)
		return
	}
	if err := u.cfg.Client.Purge(ctx, id); err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	http.Redirect(w, r, "/fleet", http.StatusSeeOther)
}

func (u *UI) mintCode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	code, err := u.cfg.Client.MintCode(ctx, id)
	if err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	u.renderDevice(w, r, id, "New enrolment code. Any earlier code no longer works; this one is valid until "+code.ExpiresAt.Local().Format("15:04")+".", "", nil, &code)
}

func (u *UI) cancelCode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.CancelCode(ctx, id); err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	u.redirectDevice(w, r, id, "Enrolment code cancelled.")
}

func (u *UI) setConfig(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	var ssids []string
	for _, line := range strings.Split(strings.ReplaceAll(r.FormValue("ssids"), ",", "\n"), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			ssids = append(ssids, s)
		}
	}
	if ssids == nil {
		ssids = []string{}
	}
	if _, err := u.cfg.Client.SetConfig(ctx, id, ssids, r.FormValue("config_version")); err != nil {
		u.renderDevice(w, r, id, "", describe(err), map[string]string{"ssids": r.FormValue("ssids")}, nil)
		return
	}
	msg := "Wi-Fi networks saved and pushed to the phone."
	if len(ssids) == 0 {
		msg = "Wi-Fi list cleared: the phone will stop waking in the background."
	}
	u.redirectDevice(w, r, id, msg)
}

func (u *UI) setLine(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	dn, du, secret := strings.TrimSpace(r.FormValue("dn")), strings.TrimSpace(r.FormValue("digest_user")), r.FormValue("secret")
	form := map[string]string{"dn": dn, "digest_user": du}
	if _, err := u.cfg.Client.SetLine(ctx, id, dn, du, secret, r.FormValue("updated_at")); err != nil {
		u.renderDevice(w, r, id, "", describe(err), form, nil)
		return
	}
	u.redirectDevice(w, r, id, "PBX line saved; the server is registering it now.")
}

func (u *UI) deleteLine(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.DeleteLine(ctx, id, r.FormValue("updated_at")); err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	u.redirectDevice(w, r, id, "PBX line removed and unregistered.")
}

// redirectDevice goes back to the device page with a flash, via a short
// query parameter so the flash survives the redirect and a reload does
// not repeat the action.
func (u *UI) redirectDevice(w http.ResponseWriter, r *http.Request, id, flash string) {
	key := u.cfg.Sessions.Stash("FLASH", "", map[string][]string{"msg": {flash}})
	http.Redirect(w, r, "/devices/"+id+"?flash="+key, http.StatusSeeOther)
}

// flashFor reads a flash left by redirectDevice.
func (u *UI) flashFor(r *http.Request) string {
	if key := r.URL.Query().Get("flash"); key != "" {
		if p, ok := u.cfg.Sessions.Take(key); ok && p.Method == "FLASH" {
			return p.Form.Get("msg")
		}
	}
	return ""
}

// ---- diagnostics -----------------------------------------------------------

func (u *UI) diagFile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	resp, err := u.cfg.Client.DiagFile(ctx, r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		http.Error(w, describe(err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	copyBody(w, resp)
}

func (u *UI) deleteDiag(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.DeleteDiag(ctx, id, r.PathValue("name")); err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	u.redirectDevice(w, r, id, "Diagnostic file deleted.")
}

func (u *UI) deleteAllDiag(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.DeleteDiag(ctx, id, ""); err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	u.redirectDevice(w, r, id, "All diagnostic files deleted.")
}
