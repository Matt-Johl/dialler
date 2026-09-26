package adminui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"dialler/server/internal/status"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Config builds the UI.
type Config struct {
	Client   *Client
	Sessions *Sessions
	// QR renders an enrolment link as inline SVG; nil shows the link only.
	QR func(link string) (template.HTML, error)
	// Timeout bounds each API call made while rendering a page.
	Timeout time.Duration
	Logger  *slog.Logger
	// Version is shown in the footer.
	Version string
}

// UI is the http.Handler.
type UI struct {
	cfg  Config
	mux  *http.ServeMux
	tmpl *template.Template

	// last is what each GET page last rendered from, shown with its time
	// when the call server is not answering (ADMIN-API.md §9).
	lastMu sync.Mutex
	last   map[string]lastFetch
}

type lastFetch struct {
	at   time.Time
	data any
}

// page is what every template receives.
type page struct {
	Title    string
	Nav      string // server, clients, calls, diagnostics
	CSRF     string
	Version  string
	Banner   string // the call server is not answering
	ReadOnly bool
	LastAt   time.Time
	Flash    string // one line of good news
	Error    string // one line of bad news
	Data     any
}

// New builds the UI.
func New(cfg Config) (*UI, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	funcs := template.FuncMap{
		"since": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			d := time.Since(t).Round(time.Second)
			switch {
			case d < time.Minute:
				return fmt.Sprintf("%ds", int(d.Seconds()))
			case d < time.Hour:
				return fmt.Sprintf("%dm", int(d.Minutes()))
			case d < 48*time.Hour:
				return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
			}
			return fmt.Sprintf("%dd", int(d.Hours()/24))
		},
		"when": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("2006-01-02 15:04:05")
		},
		"clock": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.Local().Format("15:04")
		},
		"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) },
		"join":    strings.Join,
		"lower":   strings.ToLower,
		"list":    func(a ...string) []string { return a },
		"initials": func(description, user string) string {
			var out []rune
			for _, w := range strings.Fields(description) {
				r := []rune(w)[0]
				if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
					continue
				}
				out = append(out, unicode.ToUpper(r))
				if len(out) == 2 {
					break
				}
			}
			if len(out) == 0 {
				return user
			}
			return string(out)
		},
		"seconds": func(n any) string {
			var d time.Duration
			switch v := n.(type) {
			case int:
				d = time.Duration(v)
			case int64:
				d = time.Duration(v)
			}
			d *= time.Second
			switch {
			case d == 0:
				return "off"
			case d >= 48*time.Hour && d%(24*time.Hour) == 0:
				return fmt.Sprintf("%d days", int(d.Hours()/24))
			case d >= time.Hour && d%time.Hour == 0:
				return fmt.Sprintf("%d h", int(d.Hours()))
			}
			return d.String()
		},
	}
	t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	u := &UI{cfg: cfg, tmpl: t, last: map[string]lastFetch{}}
	u.routes()
	return u, nil
}

func (u *UI) ServeHTTP(w http.ResponseWriter, r *http.Request) { u.mux.ServeHTTP(w, r) }

func (u *UI) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	m.HandleFunc("GET /login", u.loginPage)
	m.HandleFunc("POST /login", u.login)
	m.HandleFunc("POST /logout", u.logout)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/clients", http.StatusFound) })
	// Old names, kept as redirects.
	m.HandleFunc("GET /fleet", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/clients", http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/clients", http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/clients/"+url.PathEscape(r.PathValue("id")), http.StatusMovedPermanently)
	})

	m.HandleFunc("GET /server", u.auth(u.server))
	m.HandleFunc("GET /clients", u.auth(u.clients))
	m.HandleFunc("GET /clients/new", u.auth(u.newClient))
	m.HandleFunc("POST /clients", u.auth(u.createClient))
	m.HandleFunc("GET /clients/{id}", u.auth(u.client))
	m.HandleFunc("GET /clients/{id}/code", u.auth(u.codePage))
	m.HandleFunc("POST /clients/{id}/code", u.auth(u.mintCode))
	m.HandleFunc("POST /clients/{id}/code/cancel", u.auth(u.cancelCode))
	m.HandleFunc("POST /clients/{id}/description", u.auth(u.setDescription))
	m.HandleFunc("POST /clients/{id}/revoke", u.auth(u.revoke))
	m.HandleFunc("POST /clients/{id}/purge", u.auth(u.purge))
	m.HandleFunc("POST /clients/{id}/config", u.auth(u.setConfig))
	m.HandleFunc("POST /clients/{id}/line", u.auth(u.setLine))
	m.HandleFunc("POST /clients/{id}/line/delete", u.auth(u.deleteLine))
	m.HandleFunc("POST /clients/{id}/directory", u.auth(u.addContact))
	m.HandleFunc("POST /clients/{id}/directory/upload", u.auth(u.uploadCSV))
	m.HandleFunc("POST /clients/{id}/directory/copy", u.auth(u.copyDirectory))
	m.HandleFunc("GET /clients/{id}/directory.csv", u.auth(u.downloadCSV))
	m.HandleFunc("POST /clients/{id}/directory/{cid}", u.auth(u.editContact))
	m.HandleFunc("GET /clients/{id}/diag/{name}", u.auth(u.diagFile))
	m.HandleFunc("POST /clients/{id}/diag/{name}/delete", u.auth(u.deleteDiag))
	m.HandleFunc("POST /clients/{id}/diag/delete-all", u.auth(u.deleteAllDiag))
	m.HandleFunc("GET /calls", u.auth(u.calls))
	m.HandleFunc("GET /diagnostics", u.auth(u.diagnostics))
	m.HandleFunc("POST /diagnostics/log", u.auth(u.setLog))
	u.mux = m
}

// ---- auth ------------------------------------------------------------------

type ctxKey int

const (
	ctxCSRF ctxKey = iota
	ctxReplay
)

// auth requires a live session. A GET without one goes to the login page.
// A POST without one is stashed and replayed after login, so a form
// filled in on an expired session is not lost (ADMIN-API.md §9). A POST
// with a session must carry the session's CSRF token.
func (u *UI) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		csrf, ok := u.cfg.Sessions.Check(sessionID(r))
		if !ok {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
				return
			}
			if err := r.ParseMultipartForm(MaxCSVBytes + 64<<10); err != nil && !errors.Is(err, http.ErrNotMultipart) {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			form := r.Form
			if r.MultipartForm != nil {
				// Uploaded files cannot be stashed; the operator re-attaches.
				form = r.MultipartForm.Value
			}
			key := u.cfg.Sessions.Stash(r.Method, r.URL.RequestURI(), form)
			u.render(w, "login.html", page{Title: "Sign in", Error: "Your session had expired. Sign in and what you submitted will be applied.", Data: map[string]string{"Resume": key}})
			return
		}
		if r.Method != http.MethodGet {
			if r.Context().Value(ctxReplay) == nil {
				if err := r.ParseMultipartForm(MaxCSVBytes + 64<<10); err != nil && !errors.Is(err, http.ErrNotMultipart) {
					http.Error(w, "bad form", http.StatusBadRequest)
					return
				}
				if !u.cfg.Sessions.CSRFMatches(sessionID(r), r.FormValue("csrf")) {
					http.Error(w, "the form's token did not match this session; reload the page and try again", http.StatusForbidden)
					return
				}
			}
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxCSRF, csrf)))
	}
}

func (u *UI) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := u.cfg.Sessions.Check(sessionID(r)); ok {
		http.Redirect(w, r, "/clients", http.StatusFound)
		return
	}
	u.render(w, "login.html", page{Title: "Sign in", Data: map[string]string{"Next": r.URL.Query().Get("next")}})
}

func (u *UI) login(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id, ok := u.cfg.Sessions.Login(strings.TrimSpace(r.FormValue("username")), r.FormValue("password"))
	if !ok {
		time.Sleep(500 * time.Millisecond)
		u.render(w, "login.html", page{Title: "Sign in", Error: "That username and password are not right.", Data: map[string]string{"Next": r.FormValue("next"), "Resume": r.FormValue("resume"), "Username": r.FormValue("username")}})
		return
	}
	http.SetCookie(w, cookie(id))
	if key := r.FormValue("resume"); key != "" {
		if p, ok := u.cfg.Sessions.Take(key); ok {
			u.replay(w, r, id, p)
			return
		}
	}
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/clients"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

// replay re-submits a stashed form under the new session.
func (u *UI) replay(w http.ResponseWriter, r *http.Request, sessionID string, p *Pending) {
	req, err := http.NewRequestWithContext(context.WithValue(r.Context(), ctxReplay, true), p.Method, p.Path, strings.NewReader(p.Form.Encode()))
	if err != nil {
		http.Redirect(w, r, "/clients", http.StatusFound)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie(sessionID))
	u.mux.ServeHTTP(w, req)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	u.cfg.Sessions.Logout(sessionID(r))
	http.SetCookie(w, clearCookie())
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---- rendering -------------------------------------------------------------

func (u *UI) render(w http.ResponseWriter, name string, p page) {
	if p.Version == "" {
		p.Version = u.cfg.Version
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'")
	var buf strings.Builder
	if err := u.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		u.cfg.Logger.Error("template", "name", name, "err", err)
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte(buf.String()))
}

// csrf is the session's token, put in every form.
func csrf(r *http.Request) string {
	v, _ := r.Context().Value(ctxCSRF).(string)
	return v
}

func (u *UI) ctx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), u.cfg.Timeout)
}

// fetched records a page's data for the banner case, or returns the last
// good one when the server did not answer. banner is set accordingly.
func (u *UI) fetched(key string, data any, err error) (any, time.Time, string) {
	u.lastMu.Lock()
	defer u.lastMu.Unlock()
	if err == nil {
		u.last[key] = lastFetch{at: time.Now(), data: data}
		return data, time.Time{}, ""
	}
	if Unreachable(err) {
		if l, ok := u.last[key]; ok {
			return l.data, l.at, ErrUnreachable.Error()
		}
		return nil, time.Time{}, ErrUnreachable.Error()
	}
	return nil, time.Time{}, ""
}

// describe turns an API error into the line the operator sees.
func describe(err error) string {
	var ae *APIError
	switch {
	case err == nil:
		return ""
	case Unreachable(err):
		return ErrUnreachable.Error() + "; nothing was changed."
	case errors.Is(err, ErrUnauthorized):
		return "The call server refused dialler-admin's token; check -admin-token-file on both sides."
	case errors.As(err, &ae):
		switch ae.Status {
		case http.StatusPreconditionFailed:
			return "Changed by someone else since you opened this. Reload and try again."
		case http.StatusNotFound:
			return "Not found: " + ae.Message
		}
		if ae.Field != "" {
			return fmt.Sprintf("%s (%s)", ae.Message, ae.Field)
		}
		return ae.Message
	}
	return err.Error()
}

// flash stashes a one-line message for the page after a redirect, so a
// reload never repeats the action.
func (u *UI) flash(msg string) string {
	return "?flash=" + u.cfg.Sessions.Stash("FLASH", "", map[string][]string{"msg": {msg}})
}

// flashFor reads a flash left by flash().
func (u *UI) flashFor(r *http.Request) string {
	if key := r.URL.Query().Get("flash"); key != "" {
		if p, ok := u.cfg.Sessions.Take(key); ok && p.Method == "FLASH" {
			return p.Form.Get("msg")
		}
	}
	return ""
}

// ---- clients (the list) ----------------------------------------------------

// ClientRow is one client on the list: the record merged with its live
// state.
type ClientRow struct {
	status.DeviceStatus
	IssuedAt      time.Time
	CodePending   bool
	CodeExpiresAt time.Time
	HasConfig     bool
	HasLine       bool
}

type clientsData struct {
	Rows   []ClientRow
	Trunk  status.FleetView
	Counts map[string]int
}

func (u *UI) clients(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	data, err := u.loadClients(ctx)
	got, lastAt, banner := u.fetched("clients", data, err)
	p := page{Title: "Clients", Nav: "clients", CSRF: csrf(r), Banner: banner, ReadOnly: banner != "", LastAt: lastAt, Flash: u.flashFor(r)}
	if got != nil {
		p.Data = got.(clientsData)
	} else if err != nil && banner == "" {
		p.Error = describe(err)
	}
	u.render(w, "clients.html", p)
}

func (u *UI) loadClients(ctx context.Context) (clientsData, error) {
	devices, err := u.cfg.Client.Devices(ctx)
	if err != nil {
		return clientsData{}, err
	}
	st, err := u.cfg.Client.Status(ctx)
	if err != nil {
		return clientsData{}, err
	}
	live := map[string]status.DeviceStatus{}
	for _, d := range st.Devices {
		live[d.DeviceID] = d
	}
	d := clientsData{Trunk: st, Counts: map[string]int{}}
	for _, dev := range devices {
		row := ClientRow{DeviceStatus: live[dev.DeviceID], IssuedAt: dev.IssuedAt, CodePending: dev.CodePending, CodeExpiresAt: dev.CodeExpiresAt, HasConfig: dev.Config != nil, HasLine: dev.PBXLine != nil}
		row.DeviceID, row.User, row.Description, row.Revoked, row.Enrolled = dev.DeviceID, dev.User, dev.Description, dev.Revoked, dev.Enrolled
		if row.Sessions == nil {
			row.Sessions = map[string]*status.Session{}
		}
		d.Rows = append(d.Rows, row)
		d.Counts["clients"]++
		if dev.Revoked {
			d.Counts["revoked"]++
		}
		if row.Sessions["app"] != nil || row.Sessions["extension"] != nil {
			d.Counts["online"]++
		}
		if row.SIP != nil {
			d.Counts["registered"]++
		}
		if row.Call != nil {
			d.Counts["calls"]++
		}
	}
	sort.Slice(d.Rows, func(i, j int) bool { return d.Rows[i].User < d.Rows[j].User })
	return d, nil
}

// ---- server ----------------------------------------------------------------

type serverData struct {
	Server status.ServerView
}

func (u *UI) server(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	sv, err := u.cfg.Client.Server(ctx)
	got, lastAt, banner := u.fetched("server", serverData{Server: sv}, err)
	p := page{Title: "Server", Nav: "server", CSRF: csrf(r), Banner: banner, ReadOnly: banner != "", LastAt: lastAt}
	if got != nil {
		p.Data = got.(serverData)
	} else if err != nil && banner == "" {
		p.Error = describe(err)
	}
	u.render(w, "server.html", p)
}

// ---- calls -----------------------------------------------------------------

func (u *UI) calls(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	calls, err := u.cfg.Client.Calls(ctx)
	got, lastAt, banner := u.fetched("calls", calls, err)
	p := page{Title: "Calls", Nav: "calls", CSRF: csrf(r), Banner: banner, ReadOnly: banner != "", LastAt: lastAt, Data: got}
	if got == nil && err != nil && banner == "" {
		p.Error = describe(err)
	}
	u.render(w, "calls.html", p)
}

// ---- diagnostics: logging, uploads, events -----------------------------------

type diagnosticsData struct {
	Log struct {
		loglevelView
		Err string
	}
	Events  []eventRow
	Uploads []uploadRow
	Form    map[string]string
}

type eventRow struct {
	At     time.Time
	Kind   string
	Device string
	User   string
	Detail string
}

// uploadRow is one client's diagnostic files.
type uploadRow struct {
	DeviceID    string
	User        string
	Description string
	Files       int
	Latest      string
}

func (u *UI) diagnostics(w http.ResponseWriter, r *http.Request) {
	u.renderDiagnostics(w, r, "", nil)
}

func (u *UI) renderDiagnostics(w http.ResponseWriter, r *http.Request, errMsg string, form map[string]string) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	data, err := u.loadDiagnostics(ctx)
	got, lastAt, banner := u.fetched("diagnostics", data, err)
	p := page{Title: "Diagnostics", Nav: "diagnostics", CSRF: csrf(r), Banner: banner, ReadOnly: banner != "", LastAt: lastAt, Error: errMsg, Flash: u.flashFor(r)}
	if got != nil {
		d := got.(diagnosticsData)
		d.Form = form
		p.Data = d
	} else if err != nil && banner == "" {
		p.Error = describe(err)
	}
	u.render(w, "diagnostics.html", p)
}

func (u *UI) loadDiagnostics(ctx context.Context) (diagnosticsData, error) {
	var d diagnosticsData
	if lv, err := u.cfg.Client.Log(ctx); err == nil {
		d.Log.loglevelView = loglevelView{lv}
	} else if Unreachable(err) {
		return d, err
	} else {
		d.Log.Err = describe(err)
	}
	ev, err := u.cfg.Client.Events(ctx, 0, 100)
	if err != nil {
		return d, err
	}
	for i := len(ev.Events) - 1; i >= 0; i-- { // newest first
		e := ev.Events[i]
		parts := make([]string, 0, len(e.Detail))
		for k, v := range e.Detail {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
		sort.Strings(parts)
		d.Events = append(d.Events, eventRow{At: e.At, Kind: e.Kind, Device: e.DeviceID, User: e.User, Detail: strings.Join(parts, " ")})
	}
	devices, err := u.cfg.Client.Devices(ctx)
	if err != nil {
		return d, err
	}
	for _, dev := range devices {
		files, err := u.cfg.Client.Diag(ctx, dev.DeviceID)
		if err != nil {
			if Unreachable(err) {
				return d, err
			}
			continue
		}
		if len(files) == 0 {
			continue
		}
		d.Uploads = append(d.Uploads, uploadRow{DeviceID: dev.DeviceID, User: dev.User, Description: dev.Description, Files: len(files), Latest: files[0].At})
	}
	sort.Slice(d.Uploads, func(i, j int) bool { return d.Uploads[i].User < d.Uploads[j].User })
	return d, nil
}

func (u *UI) setLog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	form := map[string]string{"level": r.FormValue("level"), "sip_trace": r.FormValue("sip_trace"), "for_seconds": r.FormValue("for_seconds")}
	req := LogRequest{}
	if l := r.FormValue("level"); l != "" {
		req.Level = &l
	}
	trace := r.FormValue("sip_trace") == "on"
	req.SIPTrace = &trace
	if s := strings.TrimSpace(r.FormValue("for_seconds")); s != "" {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
			u.renderDiagnostics(w, r, "Revert after must be a whole number of seconds.", form)
			return
		}
		req.ForSeconds = &n
	}
	if _, err := u.cfg.Client.SetLog(ctx, req); err != nil {
		u.renderDiagnostics(w, r, describe(err), form)
		return
	}
	http.Redirect(w, r, "/diagnostics"+u.flash("Logging updated."), http.StatusSeeOther)
}
