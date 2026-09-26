package adminui

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"dialler/server/internal/diag"
	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/loglevel"
	"dialler/server/internal/status"
)

type loglevelView struct{ loglevel.View }

// clientData is the client page.
type clientData struct {
	Device    enroll.Device
	Live      status.DeviceStatus
	Directory directory.ListResponse
	Diag      []diag.Entry
	Others    []enroll.Device // copy-to targets
	Form      map[string]string
	Server    status.ServerView
}

func (u *UI) client(w http.ResponseWriter, r *http.Request) {
	u.renderClient(w, r, r.PathValue("id"), u.flashFor(r), "", nil)
}

func (u *UI) renderClient(w http.ResponseWriter, r *http.Request, id, flash, errMsg string, form map[string]string) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	data, err := u.loadClient(ctx, id)
	got, lastAt, banner := u.fetched("client:"+id, data, err)
	p := page{Title: "Client", Nav: "clients", CSRF: csrf(r), Banner: banner, ReadOnly: banner != "", LastAt: lastAt, Flash: flash, Error: errMsg}
	if got != nil {
		d := got.(clientData)
		d.Form = form
		p.Title = d.Device.User + " · " + d.Device.Description
		p.Data = d
	} else if err != nil && banner == "" {
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		p.Error = describe(err)
	}
	u.render(w, "client.html", p)
}

func isNotFound(err error) bool {
	var ae *APIError
	return asAPIError(err, &ae) && ae.Status == http.StatusNotFound
}

func (u *UI) loadClient(ctx context.Context, id string) (clientData, error) {
	var d clientData
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

// redirectClient goes back to the client page with a flash.
func (u *UI) redirectClient(w http.ResponseWriter, r *http.Request, id, flash string) {
	http.Redirect(w, r, "/clients/"+url.PathEscape(id)+u.flash(flash), http.StatusSeeOther)
}

// ---- add a client ----------------------------------------------------------

type newClientData struct {
	Form map[string]string
}

func (u *UI) newClient(w http.ResponseWriter, r *http.Request) {
	u.render(w, "client_new.html", page{Title: "Add a client", Nav: "clients", CSRF: csrf(r), Data: newClientData{}})
}

// createClient adds the client and goes to its enrolment page, carrying
// the freshly minted code there once (the server never shows it again).
func (u *UI) createClient(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	user, desc := strings.TrimSpace(r.FormValue("user")), strings.TrimSpace(r.FormValue("description"))
	form := map[string]string{"user": user, "description": desc}
	created, err := u.cfg.Client.CreateDevice(ctx, user, desc)
	if err != nil {
		u.render(w, "client_new.html", page{Title: "Add a client", Nav: "clients", CSRF: csrf(r), Error: describe(err), Data: newClientData{Form: form}})
		return
	}
	u.redirectToCode(w, r, created.DeviceID, Code{Code: created.Code, ExpiresAt: created.ExpiresAt, URL: created.URL})
}

// ---- the enrolment page ----------------------------------------------------

// codeData is the enrolment page: the client, and the code when this is
// the one visit that can show it.
type codeData struct {
	Device enroll.Device
	Server status.ServerView
	Code   *Code
	QR     template.HTML
}

// redirectToCode stashes a just-minted code under a one-time key and goes
// to the enrolment page, which takes it. A reload of that page has no
// code to show and offers a new one.
func (u *UI) redirectToCode(w http.ResponseWriter, r *http.Request, id string, code Code) {
	key := u.cfg.Sessions.Stash("CODE", id, url.Values{"code": {code.Code}, "url": {code.URL}, "expires": {code.ExpiresAt.UTC().Format(time.RFC3339)}})
	http.Redirect(w, r, "/clients/"+url.PathEscape(id)+"/code?k="+key, http.StatusSeeOther)
}

func (u *UI) codePage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	dev, err := u.cfg.Client.Device(ctx, id)
	if err != nil {
		if isNotFound(err) {
			http.NotFound(w, r)
			return
		}
		u.render(w, "client_code.html", page{Title: "Enrol", Nav: "clients", CSRF: csrf(r), Error: describe(err), Banner: bannerFor(err), ReadOnly: Unreachable(err)})
		return
	}
	sv, _ := u.cfg.Client.Server(ctx)
	d := codeData{Device: dev, Server: sv}
	if key := r.URL.Query().Get("k"); key != "" {
		if p, ok := u.cfg.Sessions.Take(key); ok && p.Method == "CODE" && p.Path == id {
			exp, _ := time.Parse(time.RFC3339, p.Form.Get("expires"))
			d.Code = &Code{Code: p.Form.Get("code"), URL: p.Form.Get("url"), ExpiresAt: exp}
			if u.cfg.QR != nil {
				if svg, err := u.cfg.QR(d.Code.URL); err == nil {
					d.QR = svg
				}
			}
		}
	}
	u.render(w, "client_code.html", page{Title: "Enrol " + dev.User, Nav: "clients", CSRF: csrf(r), Data: d})
}

func bannerFor(err error) string {
	if Unreachable(err) {
		return ErrUnreachable.Error()
	}
	return ""
}

func (u *UI) mintCode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	code, err := u.cfg.Client.MintCode(ctx, id)
	if err != nil {
		u.renderClient(w, r, id, "", describe(err), nil)
		return
	}
	u.redirectToCode(w, r, id, code)
}

func (u *UI) cancelCode(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.CancelCode(ctx, id); err != nil {
		u.renderClient(w, r, id, "", describe(err), nil)
		return
	}
	u.redirectClient(w, r, id, "Enrolment code cancelled.")
}

// ---- client actions --------------------------------------------------------

func (u *UI) setDescription(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	desc := strings.TrimSpace(r.FormValue("description"))
	if _, err := u.cfg.Client.SetDescription(ctx, id, desc, r.FormValue("updated_at")); err != nil {
		u.renderClient(w, r, id, "", describe(err), map[string]string{"description": desc})
		return
	}
	u.redirectClient(w, r, id, "Description saved.")
}

func (u *UI) revoke(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.Revoke(ctx, id); err != nil {
		u.renderClient(w, r, id, "", describe(err), nil)
		return
	}
	u.redirectClient(w, r, id, "Revoked: the phone can no longer connect or register. Its directory, settings and line are kept; enrol it again with a new code.")
}

func (u *UI) purge(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if r.FormValue("confirm") != id {
		u.renderClient(w, r, id, "", "To purge, type the client id into the confirmation box.", nil)
		return
	}
	if err := u.cfg.Client.Purge(ctx, id); err != nil {
		u.renderClient(w, r, id, "", describe(err), nil)
		return
	}
	http.Redirect(w, r, "/clients"+u.flash("Client "+id+" purged."), http.StatusSeeOther)
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
		u.renderClient(w, r, id, "", describe(err), map[string]string{"ssids": r.FormValue("ssids")})
		return
	}
	msg := "Wi-Fi networks saved and pushed to the phone."
	if len(ssids) == 0 {
		msg = "Wi-Fi list cleared: the phone will stop waking in the background."
	}
	u.redirectClient(w, r, id, msg)
}

func (u *UI) setLine(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	dn, du, secret := strings.TrimSpace(r.FormValue("dn")), strings.TrimSpace(r.FormValue("digest_user")), r.FormValue("secret")
	form := map[string]string{"dn": dn, "digest_user": du}
	if _, err := u.cfg.Client.SetLine(ctx, id, dn, du, secret, r.FormValue("updated_at")); err != nil {
		u.renderClient(w, r, id, "", describe(err), form)
		return
	}
	u.redirectClient(w, r, id, "PBX line saved; the server is registering it now.")
}

func (u *UI) deleteLine(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.DeleteLine(ctx, id, r.FormValue("updated_at")); err != nil {
		u.renderClient(w, r, id, "", describe(err), nil)
		return
	}
	u.redirectClient(w, r, id, "PBX line removed and unregistered.")
}

// ---- diagnostic files ------------------------------------------------------

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
		u.renderClient(w, r, id, "", describe(err), nil)
		return
	}
	u.redirectClient(w, r, id, "Diagnostic file deleted.")
}

func (u *UI) deleteAllDiag(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	if err := u.cfg.Client.DeleteDiag(ctx, id, ""); err != nil {
		u.renderClient(w, r, id, "", describe(err), nil)
		return
	}
	u.redirectClient(w, r, id, "All diagnostic files deleted.")
}
