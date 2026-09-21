package adminui

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"dialler/server/internal/csvdir"
	"dialler/server/internal/directory"
	"dialler/server/internal/enroll"
	"dialler/server/internal/pbxconfig"
	"dialler/server/internal/qr"
	"dialler/server/internal/status"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Config is what the app needs to run.
type Config struct {
	Client *Client
	// PasswordHash is the operator's password as HashPassword stores it.
	PasswordHash string
	// Secure marks the session cookie Secure: true when served over TLS.
	Secure bool
	Now    func() time.Time
	Logger *slog.Logger
}

// App is the web interface. It is an http.Handler.
type App struct {
	cfg      Config
	sessions *sessions
	logins   *loginLimiter
	pages    map[string]*template.Template
	mux      *http.ServeMux
}

// New builds the app; templates are parsed once, so a broken one fails
// here rather than on a page.
func New(cfg Config) (*App, error) {
	if cfg.Client == nil {
		return nil, errors.New("adminui: a client is required")
	}
	if cfg.PasswordHash == "" {
		return nil, errors.New("adminui: a password hash is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	a := &App{cfg: cfg, sessions: newSessions(cfg.Now), logins: &loginLimiter{now: cfg.Now, seen: map[string][]time.Time{}}, pages: map[string]*template.Template{}}
	pages, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		if strings.HasSuffix(p, "layout.html") {
			continue
		}
		t, err := template.New("layout.html").Funcs(a.funcs()).ParseFS(assets, "templates/layout.html", p)
		if err != nil {
			return nil, fmt.Errorf("adminui: %s: %w", p, err)
		}
		a.pages[strings.TrimPrefix(p, "templates/")] = t
	}
	a.routes()
	return a, nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

func (a *App) routes() {
	m := http.NewServeMux()
	a.mux = m
	static, _ := fs.Sub(assets, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /login", a.loginPage)
	m.HandleFunc("POST /login", a.login)
	m.HandleFunc("POST /logout", a.signedIn(a.logout))

	m.HandleFunc("GET /{$}", a.signedIn(a.devices))
	m.HandleFunc("GET /devices/new", a.signedIn(a.newDevicePage))
	m.HandleFunc("POST /devices", a.signedIn(a.createDevice))
	m.HandleFunc("GET /devices/{id}", a.signedIn(a.device))
	m.HandleFunc("POST /devices/{id}", a.signedIn(a.saveDevice))
	m.HandleFunc("GET /devices/{id}/delete", a.signedIn(a.deletePage))
	m.HandleFunc("POST /devices/{id}/delete", a.signedIn(a.purge))
	m.HandleFunc("GET /devices/{id}/contacts/new", a.signedIn(a.contactPage))
	m.HandleFunc("GET /devices/{id}/contacts/{cid}/edit", a.signedIn(a.contactPage))
	m.HandleFunc("GET /devices/{id}/directory/copy", a.signedIn(a.copyPage))
	m.HandleFunc("GET /devices/{id}/code", a.signedIn(a.codePage))
	m.HandleFunc("POST /devices/{id}/code", a.signedIn(a.newCode))
	m.HandleFunc("GET /devices/{id}/revoke", a.signedIn(a.revokePage))
	m.HandleFunc("POST /devices/{id}/revoke", a.signedIn(a.revoke))
	m.HandleFunc("POST /devices/{id}/config", a.signedIn(a.setConfig))
	m.HandleFunc("POST /devices/{id}/contacts", a.signedIn(a.saveContact))
	m.HandleFunc("POST /devices/{id}/contacts/{cid}", a.signedIn(a.saveContact))
	m.HandleFunc("POST /devices/{id}/contacts/{cid}/delete", a.signedIn(a.deleteContact))
	m.HandleFunc("GET /devices/{id}/directory.csv", a.signedIn(a.downloadCSV))
	m.HandleFunc("POST /devices/{id}/directory/upload", a.signedIn(a.uploadCSV))
	m.HandleFunc("POST /devices/{id}/directory/apply", a.signedIn(a.applyCSV))
	m.HandleFunc("POST /devices/{id}/directory/copy", a.signedIn(a.copyDirectory))
	m.HandleFunc("GET /server", a.signedIn(a.server))
	m.HandleFunc("POST /server/pbx", a.signedIn(a.savePBX))
	m.HandleFunc("POST /devices/{id}/pbx/clear", a.signedIn(a.clearDevicePBX))
}

// MARK: sessions

type handler func(w http.ResponseWriter, r *http.Request, sess *session)

// signedIn requires a live session, and on every POST a CSRF token that
// matches it.
func (a *App) signedIn(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := a.session(r)
		if sess == nil {
			next := url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && r.FormValue("_csrf") != a.sessions.csrf(sess) {
			a.fail(w, r, http.StatusForbidden, "That form was not from this session. Go back and try again.")
			return
		}
		h(w, r, sess)
	}
}

func (a *App) session(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	return a.sessions.get(c.Value)
}

func (a *App) setSessionCookie(w http.ResponseWriter, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, Secure: a.cfg.Secure,
		SameSite: http.SameSiteStrictMode, MaxAge: maxAge,
	})
}

// MARK: pages

type base struct {
	Title  string
	CSRF   string
	Notice string
	Error  string
	Path   string
	// SignedIn hides the navigation on the login page.
	SignedIn bool
}

func (a *App) render(w http.ResponseWriter, r *http.Request, sess *session, page string, data any) {
	t, ok := a.pages[page]
	if !ok {
		a.fail(w, r, http.StatusInternalServerError, "missing page "+page)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		a.cfg.Logger.Error("render", "page", page, "err", err)
		a.fail(w, r, http.StatusInternalServerError, "The page could not be rendered.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'")
	_, _ = buf.WriteTo(w)
}

func (a *App) base(sess *session, title, path string) base {
	b := base{Title: title, Path: path, SignedIn: sess != nil}
	if sess != nil {
		b.CSRF = a.sessions.csrf(sess)
		if n, ok := a.sessions.takeFlash(sess, "notice").(string); ok {
			b.Notice = n
		}
		if e, ok := a.sessions.takeFlash(sess, "error").(string); ok {
			b.Error = e
		}
	}
	return b
}

// fail shows the error page.
func (a *App) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	t := a.pages["error.html"]
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	sess := a.session(r)
	data := struct {
		base
		Status  int
		Message string
	}{a.base(sess, "Something went wrong", r.URL.Path), code, msg}
	if t != nil {
		_ = t.ExecuteTemplate(w, "layout.html", data)
	} else {
		_, _ = io.WriteString(w, msg)
	}
}

// apiFailed turns a call-server error into a flash and a redirect back.
func (a *App) apiFailed(w http.ResponseWriter, r *http.Request, sess *session, back string, err error) {
	a.cfg.Logger.Warn("call server", "path", r.URL.Path, "err", err)
	a.sessions.setFlash(sess, "error", err.Error())
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (a *App) funcs() template.FuncMap {
	return template.FuncMap{
		"grouped": func(code string) string { // A7K2M9PX → A7K2-M9PX
			if len(code) == 8 {
				return code[:4] + "-" + code[4:]
			}
			return code
		},
		"when": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.Local().Format("2 Jan 2006, 15:04")
		},
		"since": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			d := a.cfg.Now().Sub(t).Round(time.Minute)
			switch {
			case d < time.Minute:
				return "just now"
			case d < time.Hour:
				return fmt.Sprintf("%d min ago", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("%d h ago", int(d.Hours()))
			default:
				return fmt.Sprintf("%d days ago", int(d.Hours()/24))
			}
		},
		"minutesLeft": func(t time.Time) int {
			return int(t.Sub(a.cfg.Now()).Minutes())
		},
		"join":  strings.Join,
		"lower": strings.ToLower,
		"icon":  icon,
	}
}

// MARK: login

type loginView struct {
	base
	Next string
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.session(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, nil, "login.html", loginView{base: a.base(nil, "Sign in", "/login"), Next: safeNext(r.URL.Query().Get("next"))})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.logins.allow(sourceOf(r)) {
		v := loginView{base: a.base(nil, "Sign in", "/login"), Next: safeNext(r.FormValue("next"))}
		v.Error = "Too many attempts. Wait a minute and try again."
		w.WriteHeader(http.StatusTooManyRequests)
		a.render(w, r, nil, "login.html", v)
		return
	}
	if !VerifyPassword(a.cfg.PasswordHash, r.FormValue("password")) {
		v := loginView{base: a.base(nil, "Sign in", "/login"), Next: safeNext(r.FormValue("next"))}
		v.Error = "That password is not right."
		w.WriteHeader(http.StatusUnauthorized)
		a.render(w, r, nil, "login.html", v)
		return
	}
	id, _ := a.sessions.start()
	a.setSessionCookie(w, id, int(sessionIdle.Seconds()))
	http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request, _ *session) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sessions.end(c.Value)
	}
	a.setSessionCookie(w, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// safeNext keeps a post-login redirect on this site.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

// MARK: devices

// deviceRow is one device as the list and the device page show it.
type deviceRow struct {
	status.Device
	// State is what the status column says; StateKey its CSS class.
	State, StateKey string
	// Presence is the connection kinds held, or "offline".
	Presence string
}

func toRow(d status.Device) deviceRow {
	row := deviceRow{Device: d}
	switch {
	case d.Revoked:
		row.State, row.StateKey = "Revoked", "revoked"
	case !d.Enrolled && d.CodePending:
		row.State, row.StateKey = "Code issued", "pending"
	case !d.Enrolled:
		row.State, row.StateKey = "Not enrolled", "none"
	default:
		row.State, row.StateKey = "Enrolled", "enrolled"
	}
	if len(d.Online) == 0 {
		row.Presence = "offline"
	} else {
		kinds := make([]string, len(d.Online))
		for i, k := range d.Online {
			kinds[i] = string(k)
		}
		row.Presence = strings.Join(kinds, " · ")
	}
	return row
}

type devicesView struct {
	base
	Server  status.Server
	Devices []deviceRow
	Now     time.Time
}

func (a *App) devices(w http.ResponseWriter, r *http.Request, sess *session) {
	st, err := a.cfg.Client.Status(r.Context())
	v := devicesView{base: a.base(sess, "Devices", "/"), Server: st.Server, Now: st.Now}
	if err != nil {
		v.Error = err.Error()
	}
	for _, d := range st.Devices {
		v.Devices = append(v.Devices, toRow(d))
	}
	sort.SliceStable(v.Devices, func(i, j int) bool {
		if v.Devices[i].Revoked != v.Devices[j].Revoked {
			return !v.Devices[i].Revoked
		}
		return v.Devices[i].User < v.Devices[j].User
	})
	a.render(w, r, sess, "devices.html", v)
}

type newDeviceView struct {
	base
	DeviceID, User, Label string
}

func (a *App) newDevicePage(w http.ResponseWriter, r *http.Request, sess *session) {
	a.render(w, r, sess, "device-new.html", newDeviceView{base: a.base(sess, "Add a device", "/devices/new")})
}

func (a *App) createDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	v := newDeviceView{
		base:     a.base(sess, "Add a device", "/devices/new"),
		DeviceID: strings.TrimSpace(r.FormValue("device_id")),
		User:     strings.TrimSpace(r.FormValue("user")),
		Label:    strings.TrimSpace(r.FormValue("label")),
	}
	// The form comes back with what was typed and the reason, rather
	// than a bare redirect that loses it.
	reject := func(msg string) {
		v.Error = msg
		w.WriteHeader(http.StatusUnprocessableEntity)
		a.render(w, r, sess, "device-new.html", v)
	}
	switch {
	case v.User == "":
		reject("A device needs an extension: the number it answers as.")
		return
	case v.DeviceID != "" && !enroll.ValidDeviceID(v.DeviceID):
		reject("A device id is 1 to 64 letters, digits, dots, dashes or underscores.")
		return
	}
	if v.DeviceID != "" {
		// The call server treats a repeated id as an update (the harness
		// relies on that), so the console refuses one that is taken.
		if _, _, err := a.find(r.Context(), v.DeviceID); err == nil {
			reject("There is already a device " + v.DeviceID + ".")
			return
		}
	}
	created, err := a.cfg.Client.CreateDevice(r.Context(), v.DeviceID, v.User, v.Label)
	if err != nil {
		a.apiFailed(w, r, sess, "/devices/new", err)
		return
	}
	a.sessions.setFlash(sess, "notice", "Device added. Put its settings in, then issue an enrolment code.")
	http.Redirect(w, r, "/devices/"+url.PathEscape(created.DeviceID), http.StatusSeeOther)
}

// find returns one device's status row, or an error when it is unknown.
func (a *App) find(ctx context.Context, id string) (deviceRow, status.Response, error) {
	st, err := a.cfg.Client.Status(ctx)
	if err != nil {
		return deviceRow{}, st, err
	}
	for _, d := range st.Devices {
		if d.DeviceID == id {
			return toRow(d), st, nil
		}
	}
	return deviceRow{}, st, &APIError{Status: http.StatusNotFound, Body: "no such device"}
}

type deviceView struct {
	base
	Device     deviceRow
	Server     status.Server
	Contacts   []directory.Contact
	Version    int64
	Config     *enroll.DeviceConfig
	SSIDs      string
	Favourites int
	CSVColumn  string
	// RegisterMode is whether the saved PBX settings use register mode.
	RegisterMode bool
}

func (a *App) device(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	row, st, err := a.find(r.Context(), id)
	if err != nil {
		if IsNotFound(err) {
			a.fail(w, r, http.StatusNotFound, "There is no device "+id+".")
			return
		}
		a.fail(w, r, http.StatusBadGateway, err.Error())
		return
	}
	v := deviceView{base: a.base(sess, row.DeviceID, "/devices/"+id), Device: row, Server: st.Server, CSVColumn: strings.Join(csvdir.Columns, ",")}
	dir, err := a.cfg.Client.Directory(r.Context(), id)
	if err != nil {
		v.Error = err.Error()
	}
	v.Contacts, v.Version = dir.Contacts, dir.Version
	sort.SliceStable(v.Contacts, func(i, j int) bool {
		return strings.ToLower(v.Contacts[i].DisplayName) < strings.ToLower(v.Contacts[j].DisplayName)
	})
	if cfg, err := a.cfg.Client.Config(r.Context(), id); err == nil && cfg != nil {
		v.Config = cfg
		v.SSIDs = strings.Join(cfg.SSIDs, "\n")
	}
	for _, c := range v.Contacts {
		if c.Favourite {
			v.Favourites++
		}
	}
	v.RegisterMode = st.PBX != nil && st.PBX.Mode == pbxconfig.ModeRegister
	a.render(w, r, sess, "device.html", v)
}

// saveDevice applies the device page's one form: name and extension,
// networks, and PBX credentials — each only when it changed, so an
// untouched group costs nothing and touches nothing.
func (a *App) saveDevice(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	back := "/devices/" + url.PathEscape(id)
	row, _, err := a.find(r.Context(), id)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, "There is no device "+id+".")
		return
	}
	user := strings.TrimSpace(r.FormValue("user"))
	label := strings.TrimSpace(r.FormValue("label"))
	if user == "" {
		a.sessions.setFlash(sess, "error", "A device needs an extension: the number it answers as.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	var saved []string
	if user != row.User || label != row.Label {
		if _, err := a.cfg.Client.UpdateDevice(r.Context(), id, user, label); err != nil {
			a.apiFailed(w, r, sess, back, err)
			return
		}
		saved = append(saved, "name and extension")
	}
	// Networks: compare with what is saved; a field the form did not
	// carry at all is left alone.
	if _, has := r.Form["ssids"]; has {
		ssids := splitSSIDs(r.FormValue("ssids"))
		current, _ := a.cfg.Client.Config(r.Context(), id)
		changed := (current == nil && len(ssids) > 0) || (current != nil && strings.Join(current.SSIDs, "\n") != strings.Join(ssids, "\n"))
		if changed {
			if _, err := a.cfg.Client.SetConfig(r.Context(), id, ssids); err != nil {
				a.apiFailed(w, r, sess, back, err)
				return
			}
			saved = append(saved, "networks")
		}
	}
	// PBX credentials: a username sets or updates them (a blank password
	// keeps the stored one); a blanked username with credentials on file
	// removes them.
	if _, has := r.Form["pbx_user"]; has {
		pbxUser := strings.TrimSpace(r.FormValue("pbx_user"))
		switch {
		case pbxUser != "":
			creds := enroll.PBXCredentials{User: pbxUser, Password: r.FormValue("pbx_password"), DeviceName: strings.TrimSpace(r.FormValue("pbx_device_name"))}
			if row.PBX == nil && creds.Password == "" {
				a.sessions.setFlash(sess, "error", "A PBX password is needed the first time.")
				http.Redirect(w, r, back, http.StatusSeeOther)
				return
			}
			if row.PBX == nil || row.PBX.User != creds.User || row.PBX.DeviceName != creds.DeviceName || creds.Password != "" {
				if err := a.cfg.Client.SetDevicePBX(r.Context(), id, creds); err != nil {
					a.apiFailed(w, r, sess, back, err)
					return
				}
				saved = append(saved, "PBX registration")
			}
		case row.PBX != nil:
			if err := a.cfg.Client.ClearDevicePBX(r.Context(), id); err != nil && !IsNotFound(err) {
				a.apiFailed(w, r, sess, back, err)
				return
			}
			saved = append(saved, "PBX registration removed")
		}
	}
	if len(saved) == 0 {
		a.sessions.setFlash(sess, "notice", "Nothing had changed.")
	} else {
		a.sessions.setFlash(sess, "notice", "Saved: "+strings.Join(saved, ", ")+".")
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func splitSSIDs(raw string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(raw, func(c rune) bool { return c == '\n' || c == ',' }) {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// MARK: delete

func (a *App) deletePage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	row, _, err := a.find(r.Context(), id)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, "There is no device "+id+".")
		return
	}
	a.render(w, r, sess, "confirm.html", confirmView{
		base:    a.base(sess, "Delete "+deviceName(row), "/devices/"+id+"/delete"),
		Heading: "Delete " + deviceName(row) + "?",
		Message: "The device, its directory and its settings are removed for good, and the phone is disconnected. Extension " + row.User + " becomes free for a new device. There is no undo.",
		Action:  "/devices/" + url.PathEscape(id) + "/delete",
		Button:  "Delete device",
		Back:    "/devices/" + url.PathEscape(id),
	})
}

func (a *App) purge(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	if err := a.cfg.Client.Purge(r.Context(), id); err != nil && !IsNotFound(err) {
		a.apiFailed(w, r, sess, "/devices/"+url.PathEscape(id), err)
		return
	}
	a.sessions.setFlash(sess, "notice", "Device deleted.")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// MARK: enrolment codes

type codeView struct {
	base
	Device deviceRow
	Code   *Code
	QR     template.HTML
}

func (a *App) codePage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	row, _, err := a.find(r.Context(), id)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, "There is no device "+id+".")
		return
	}
	v := codeView{base: a.base(sess, "Enrol "+deviceName(row), "/devices/"+id+"/code"), Device: row}
	if c, ok := a.sessions.takeFlash(sess, "code:"+id).(Code); ok {
		v.Code = &c
		if sym, err := qr.Encode([]byte(c.URL)); err == nil {
			v.QR = template.HTML(sym.SVG()) //nolint:gosec // our own SVG, from our own encoder
		}
	}
	a.render(w, r, sess, "code.html", v)
}

func (a *App) newCode(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	c, err := a.cfg.Client.NewCode(r.Context(), id)
	if err != nil {
		a.apiFailed(w, r, sess, "/devices/"+url.PathEscape(id), err)
		return
	}
	a.sessions.setFlash(sess, "code:"+id, c)
	http.Redirect(w, r, "/devices/"+url.PathEscape(id)+"/code", http.StatusSeeOther)
}

// deviceName is how a device is referred to in titles and confirmations:
// by its id, which is what the admin chose and what the phone presents.
func deviceName(d deviceRow) string { return d.DeviceID }

// MARK: revoke

type confirmView struct {
	base
	Heading string
	Message string
	Action  string
	Button  string
	Back    string
}

func (a *App) revokePage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	row, _, err := a.find(r.Context(), id)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, "There is no device "+id+".")
		return
	}
	a.render(w, r, sess, "confirm.html", confirmView{
		base:    a.base(sess, "Revoke "+deviceName(row), "/devices/"+id+"/revoke"),
		Heading: "Revoke " + deviceName(row) + "?",
		Message: "Its credential stops working at once: the phone is disconnected and can no longer register or make calls. Its directory is kept, and a new enrolment code brings it back.",
		Action:  "/devices/" + url.PathEscape(id) + "/revoke",
		Button:  "Revoke",
		Back:    "/devices/" + url.PathEscape(id),
	})
}

func (a *App) revoke(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	if err := a.cfg.Client.Revoke(r.Context(), id); err != nil {
		a.apiFailed(w, r, sess, "/devices/"+url.PathEscape(id), err)
		return
	}
	a.sessions.setFlash(sess, "notice", "Revoked. A new enrolment code will bring the device back.")
	http.Redirect(w, r, "/devices/"+url.PathEscape(id), http.StatusSeeOther)
}

// MARK: settings

func (a *App) setConfig(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	ssids := splitSSIDs(r.FormValue("ssids"))
	if _, err := a.cfg.Client.SetConfig(r.Context(), id, ssids); err != nil {
		a.apiFailed(w, r, sess, "/devices/"+url.PathEscape(id), err)
		return
	}
	if len(ssids) == 0 {
		a.sessions.setFlash(sess, "notice", "No networks: background calls are off for this phone.")
	} else {
		a.sessions.setFlash(sess, "notice", "Networks saved and sent to the phone.")
	}
	http.Redirect(w, r, "/devices/"+url.PathEscape(id), http.StatusSeeOther)
}

// MARK: contacts

type contactView struct {
	base
	Device  deviceRow
	Contact directory.Contact
	IsNew   bool
	Action  string
}

func (a *App) contactPage(w http.ResponseWriter, r *http.Request, sess *session) {
	id, cid := r.PathValue("id"), r.PathValue("cid")
	row, _, err := a.find(r.Context(), id)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, "There is no device "+id+".")
		return
	}
	v := contactView{Device: row, IsNew: cid == "", Action: "/devices/" + url.PathEscape(id) + "/contacts"}
	v.Contact.Mode = directory.ModeLocal
	if cid != "" {
		dir, err := a.cfg.Client.Directory(r.Context(), id)
		if err != nil {
			a.apiFailed(w, r, sess, "/devices/"+url.PathEscape(id), err)
			return
		}
		found := false
		for _, c := range dir.Contacts {
			if c.ID == cid {
				v.Contact, found = c, true
			}
		}
		if !found {
			a.fail(w, r, http.StatusNotFound, "That contact is no longer in the directory.")
			return
		}
		v.Action += "/" + url.PathEscape(cid)
	}
	title := "Add a contact"
	if !v.IsNew {
		title = "Edit " + v.Contact.DisplayName
	}
	v.base = a.base(sess, title, r.URL.Path)
	a.render(w, r, sess, "contact.html", v)
}

type copyView struct {
	base
	Device deviceRow
	Others []deviceRow
	Count  int
}

func (a *App) copyPage(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	row, st, err := a.find(r.Context(), id)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, "There is no device "+id+".")
		return
	}
	v := copyView{base: a.base(sess, "Copy the directory", r.URL.Path), Device: row}
	if dir, err := a.cfg.Client.Directory(r.Context(), id); err == nil {
		v.Count = len(dir.Contacts)
	}
	for _, d := range st.Devices {
		if d.DeviceID != id && !d.Revoked {
			v.Others = append(v.Others, toRow(d))
		}
	}
	a.render(w, r, sess, "copy.html", v)
}

func (a *App) saveContact(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	back := "/devices/" + url.PathEscape(id)
	c := directory.Contact{
		ID:          r.PathValue("cid"),
		DisplayName: strings.TrimSpace(r.FormValue("display_name")),
		URI:         csvdir.NormalizeURI(r.FormValue("uri"), a.sipDomain(r.Context())),
		Mode:        directory.Mode(r.FormValue("mode")),
		Favourite:   r.FormValue("favourite") != "",
	}
	if c.DisplayName == "" || c.URI == "" {
		if wantsJSON(r) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "A contact needs a name and a number."})
			return
		}
		a.sessions.setFlash(sess, "error", "A contact needs a name and a number.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	stored, err := a.cfg.Client.UpsertContact(r.Context(), id, c)
	if err != nil {
		if wantsJSON(r) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		a.apiFailed(w, r, sess, back, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"contact": stored})
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// wantsJSON is a fetch from our own script rather than a form submit.
func wantsJSON(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "fetch"
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *App) deleteContact(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	back := "/devices/" + url.PathEscape(id)
	if err := a.cfg.Client.DeleteContact(r.Context(), id, r.PathValue("cid")); err != nil && !IsNotFound(err) {
		if wantsJSON(r) {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		a.apiFailed(w, r, sess, back, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// sipDomain is what completes a bare number; "" if the server is unreachable.
func (a *App) sipDomain(ctx context.Context) string {
	st, err := a.cfg.Client.Status(ctx)
	if err != nil {
		return ""
	}
	return st.Server.SIPDomain
}

// MARK: CSV

func (a *App) downloadCSV(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	dir, err := a.cfg.Client.Directory(r.Context(), id)
	if err != nil {
		a.apiFailed(w, r, sess, "/devices/"+url.PathEscape(id), err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-directory.csv"`, id))
	_, _ = w.Write(csvdir.Encode(dir.Contacts))
}

// pendingUpload is a parsed CSV waiting for the operator's confirmation.
type pendingUpload struct {
	Contacts []directory.Contact
	FileName string
}

type previewView struct {
	base
	Device                  deviceRow
	FileName                string
	Added, Changed, Removed []directory.Contact
	Unchanged               int
	Total                   int
}

// uploadCSV parses the file and shows what applying it would do; nothing
// is written until the operator confirms.
func (a *App) uploadCSV(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	back := "/devices/" + url.PathEscape(id)
	row, st, err := a.find(r.Context(), id)
	if err != nil {
		a.apiFailed(w, r, sess, "/", err)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		a.sessions.setFlash(sess, "error", "Choose a CSV file to upload.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	defer f.Close()
	contacts, err := csvdir.Decode(io.LimitReader(f, 4<<20), st.Server.SIPDomain)
	if err != nil {
		a.sessions.setFlash(sess, "error", err.Error())
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	current, err := a.cfg.Client.Directory(r.Context(), id)
	if err != nil {
		a.apiFailed(w, r, sess, back, err)
		return
	}
	v := previewView{base: a.base(sess, "Replace the directory", back+"/directory/upload"), Device: row, FileName: hdr.Filename, Total: len(contacts)}
	byURI := map[string]directory.Contact{}
	for _, c := range current.Contacts {
		byURI[strings.ToLower(c.URI)] = c
	}
	seen := map[string]bool{}
	for _, c := range contacts {
		key := strings.ToLower(c.URI)
		seen[key] = true
		old, ok := byURI[key]
		switch {
		case !ok:
			v.Added = append(v.Added, c)
		case old.DisplayName != c.DisplayName || old.Mode != c.Mode || old.Favourite != c.Favourite:
			v.Changed = append(v.Changed, c)
		default:
			v.Unchanged++
		}
	}
	for _, c := range current.Contacts {
		if !seen[strings.ToLower(c.URI)] {
			v.Removed = append(v.Removed, c)
		}
	}
	a.sessions.setFlash(sess, "upload:"+id, pendingUpload{Contacts: contacts, FileName: hdr.Filename})
	a.render(w, r, sess, "preview.html", v)
}

func (a *App) applyCSV(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	back := "/devices/" + url.PathEscape(id)
	pending, ok := a.sessions.takeFlash(sess, "upload:"+id).(pendingUpload)
	if !ok {
		a.sessions.setFlash(sess, "error", "Nothing is waiting to be applied. Upload the file again.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	res, err := a.cfg.Client.ReplaceDirectory(r.Context(), id, pending.Contacts)
	if err != nil {
		a.apiFailed(w, r, sess, back, err)
		return
	}
	a.sessions.setFlash(sess, "notice", fmt.Sprintf("Directory replaced from %s: %d added, %d changed, %d removed.", pending.FileName, res.Added, res.Changed, res.Removed))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// copyDirectory gives the chosen devices this device's live list.
func (a *App) copyDirectory(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	back := "/devices/" + url.PathEscape(id)
	targets := r.Form["to"]
	if len(targets) == 0 {
		a.sessions.setFlash(sess, "error", "Choose at least one device to copy to.")
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	dir, err := a.cfg.Client.Directory(r.Context(), id)
	if err != nil {
		a.apiFailed(w, r, sess, back, err)
		return
	}
	var done []string
	for _, t := range targets {
		if t == id {
			continue
		}
		if _, err := a.cfg.Client.ReplaceDirectory(r.Context(), t, dir.Contacts); err != nil {
			a.apiFailed(w, r, sess, back, fmt.Errorf("copying to %s: %w", t, err))
			return
		}
		done = append(done, t)
	}
	a.sessions.setFlash(sess, "notice", fmt.Sprintf("Directory copied to %d device(s).", len(done)))
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// MARK: server

type serverView struct {
	base
	Server  status.Server
	Devices int
	Online  int
	Now     time.Time
	// PBX is what the administrator saved; the form shows it, or defaults.
	PBX      pbxconfig.Settings
	PBXSaved bool
	// Running is the trunk the server is using now, "" when none.
	Running string
	// Restart says the saved settings and the running trunk differ.
	Restart bool
}

func (a *App) server(w http.ResponseWriter, r *http.Request, sess *session) {
	st, err := a.cfg.Client.Status(r.Context())
	v := serverView{base: a.base(sess, "Server", "/server"), Server: st.Server, Devices: len(st.Devices), Now: st.Now, Running: st.Server.Trunk}
	if err != nil {
		v.Error = err.Error()
	}
	v.PBX = pbxconfig.Settings{Mode: pbxconfig.ModePeer, Port: 5060, Transport: "udp", ExpirySeconds: 300, Codecs: "g722,pcmu,pcma", SRTP: "off", QualifySeconds: 10}
	if st.PBX != nil {
		v.PBX, v.PBXSaved = *st.PBX, true
		want := fmt.Sprintf("sip:%s:%d;transport=%s", st.PBX.Host, st.PBX.Port, st.PBX.Transport)
		v.Restart = want != st.Server.Trunk
	}
	for _, d := range st.Devices {
		if len(d.Online) > 0 {
			v.Online++
		}
	}
	a.render(w, r, sess, "server.html", v)
}

func (a *App) savePBX(w http.ResponseWriter, r *http.Request, sess *session) {
	st := pbxconfig.Settings{
		Mode:        pbxconfig.Mode(r.FormValue("mode")),
		Host:        r.FormValue("host"),
		Port:        atoi(r.FormValue("port")),
		Transport:   r.FormValue("transport"),
		Codecs:      r.FormValue("codecs"),
		SRTP:        r.FormValue("srtp"),
		TLSCert:     strings.TrimSpace(r.FormValue("tls_cert")),
		TLSKey:      strings.TrimSpace(r.FormValue("tls_key")),
		TLSCA:       strings.TrimSpace(r.FormValue("tls_ca")),
		TLSInsecure: r.FormValue("tls_insecure") != "",
	}
	st.ExpirySeconds = atoi(r.FormValue("expiry_seconds"))
	st.QualifySeconds = atoi(r.FormValue("qualify_seconds"))
	if err := st.Validate(); err != nil {
		a.sessions.setFlash(sess, "error", err.Error())
		http.Redirect(w, r, "/server", http.StatusSeeOther)
		return
	}
	if _, err := a.cfg.Client.SetPBX(r.Context(), st); err != nil {
		a.apiFailed(w, r, sess, "/server", err)
		return
	}
	a.sessions.setFlash(sess, "notice", "PBX settings saved. They take effect when the call server next starts.")
	http.Redirect(w, r, "/server", http.StatusSeeOther)
}

func atoi(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1<<30 {
			return 0
		}
	}
	return n
}

// MARK: per-device PBX credentials

func (a *App) clearDevicePBX(w http.ResponseWriter, r *http.Request, sess *session) {
	id := r.PathValue("id")
	if err := a.cfg.Client.ClearDevicePBX(r.Context(), id); err != nil && !IsNotFound(err) {
		a.apiFailed(w, r, sess, "/devices/"+url.PathEscape(id), err)
		return
	}
	a.sessions.setFlash(sess, "notice", "PBX registration removed for this extension.")
	http.Redirect(w, r, "/devices/"+url.PathEscape(id), http.StatusSeeOther)
}
