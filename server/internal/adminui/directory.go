package adminui

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"dialler/server/internal/directory"
)

var bareNumber = regexp.MustCompile(`^[0-9+*#]{1,32}$`)

// normaliseURI turns a bare number into sip:<n>@<domain> (SPEC §4.8's
// CSV rule); anything else is passed through.
func normaliseURI(uri, domain string) string {
	uri = strings.TrimSpace(uri)
	if bareNumber.MatchString(uri) && domain != "" {
		return "sip:" + uri + "@" + domain
	}
	return uri
}

func asAPIError(err error, target **APIError) bool { return errors.As(err, target) }

func (u *UI) addContact(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	sv, _ := u.cfg.Client.Server(ctx)
	c := directory.Contact{
		DisplayName: strings.TrimSpace(r.FormValue("display_name")),
		URI:         normaliseURI(r.FormValue("uri"), sv.LocalDomain),
		Mode:        directory.Mode(r.FormValue("mode")),
		Favourite:   r.FormValue("favourite") == "on",
	}
	form := map[string]string{"display_name": c.DisplayName, "uri": r.FormValue("uri"), "mode": string(c.Mode)}
	if _, err := u.cfg.Client.AddContact(ctx, id, c, r.FormValue("directory_version")); err != nil {
		u.renderDevice(w, r, id, "", describe(err), form, nil)
		return
	}
	u.redirectDevice(w, r, id, "Contact added and pushed to the phone.")
}

// editContact is save, delete, or a favourite toggle on one contact,
// chosen by the form's action field.
func (u *UI) editContact(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id, cid := r.PathValue("id"), r.PathValue("cid")
	ifMatch := r.FormValue("directory_version")
	switch r.FormValue("action") {
	case "delete":
		if err := u.cfg.Client.DeleteContact(ctx, id, cid, ifMatch); err != nil {
			u.renderDevice(w, r, id, "", describe(err), nil, nil)
			return
		}
		u.redirectDevice(w, r, id, "Contact deleted.")
	case "star", "unstar", "save":
		sv, _ := u.cfg.Client.Server(ctx)
		c := directory.Contact{
			ID:          cid,
			DisplayName: strings.TrimSpace(r.FormValue("display_name")),
			URI:         normaliseURI(r.FormValue("uri"), sv.LocalDomain),
			Mode:        directory.Mode(r.FormValue("mode")),
			Favourite:   r.FormValue("favourite") == "on",
		}
		switch r.FormValue("action") {
		case "star":
			c.Favourite = true
		case "unstar":
			c.Favourite = false
		}
		if _, err := u.cfg.Client.UpdateContact(ctx, id, c, ifMatch); err != nil {
			u.renderDevice(w, r, id, "", describe(err), nil, nil)
			return
		}
		u.redirectDevice(w, r, id, "Contact saved.")
	default:
		u.renderDevice(w, r, id, "", "unknown action", nil, nil)
	}
}

func (u *UI) downloadCSV(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	list, err := u.cfg.Client.Directory(ctx, id)
	if err != nil {
		http.Error(w, describe(err), http.StatusBadGateway)
		return
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, list.Contacts); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-directory.csv"`, id))
	_, _ = w.Write(buf.Bytes())
}

// uploadData is the upload preview page.
type uploadData struct {
	DeviceID string
	Device   string
	Result   directory.ReplaceResult
	Version  int64
	CSV      string // the file, carried to the confirm step
	Count    int
}

// uploadCSV runs in two steps: the file is parsed and previewed through
// the server's dry run, then, with the operator's confirmation, applied
// as one replace-all against the version the preview saw.
func (u *UI) uploadCSV(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	var text string
	if r.FormValue("stage") == "confirm" {
		text = r.FormValue("csv")
	} else {
		f, _, err := r.FormFile("file")
		if err != nil {
			u.renderDevice(w, r, id, "", "Choose a CSV file to upload.", nil, nil)
			return
		}
		defer f.Close()
		raw, err := io.ReadAll(io.LimitReader(f, MaxCSVBytes+1))
		if err != nil || len(raw) > MaxCSVBytes {
			u.renderDevice(w, r, id, "", "The file could not be read or is over 4 MiB.", nil, nil)
			return
		}
		text = string(raw)
	}
	sv, err := u.cfg.Client.Server(ctx)
	if err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	contacts, err := ReadCSV(strings.NewReader(text))
	if err != nil {
		u.renderDevice(w, r, id, "", "CSV: "+err.Error(), nil, nil)
		return
	}
	for i := range contacts {
		contacts[i].URI = normaliseURI(contacts[i].URI, sv.LocalDomain)
	}
	if r.FormValue("stage") == "confirm" {
		res, err := u.cfg.Client.ReplaceDirectory(ctx, id, contacts, r.FormValue("directory_version"), false)
		if err != nil {
			u.renderDevice(w, r, id, "", describe(err), nil, nil)
			return
		}
		u.redirectDevice(w, r, id, fmt.Sprintf("Directory replaced: %d added, %d changed, %d removed (version %d).", res.Added, res.Changed, res.Removed, res.Version))
		return
	}
	res, err := u.cfg.Client.ReplaceDirectory(ctx, id, contacts, "", true)
	if err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	dev, _ := u.cfg.Client.Device(ctx, id)
	u.render(w, "upload.html", page{Title: "Upload directory", Nav: "devices", CSRF: csrf(r), Data: uploadData{
		DeviceID: id, Device: dev.User + " · " + dev.Description, Result: res, Version: res.Version, CSV: text, Count: len(contacts),
	}})
}

// copyData is the copy-to results page.
type copyData struct {
	DeviceID string
	Results  []copyResult
}

type copyResult struct {
	DeviceID string
	User     string
	Result   directory.ReplaceResult
	Err      string
}

// copyDirectory replaces each chosen target's directory with this one's:
// one request per target, in order, every result shown, no rollback
// (ADMIN-API.md §9).
func (u *UI) copyDirectory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := u.ctx(r)
	defer cancel()
	id := r.PathValue("id")
	targets := r.Form["targets"]
	if len(targets) == 0 {
		u.renderDevice(w, r, id, "", "Choose at least one device to copy to.", nil, nil)
		return
	}
	src, err := u.cfg.Client.Directory(ctx, id)
	if err != nil {
		u.renderDevice(w, r, id, "", describe(err), nil, nil)
		return
	}
	contacts := make([]directory.Contact, 0, len(src.Contacts))
	for _, c := range src.Contacts {
		contacts = append(contacts, directory.Contact{DisplayName: c.DisplayName, URI: c.URI, Mode: c.Mode, Favourite: c.Favourite})
	}
	all, _ := u.cfg.Client.Devices(ctx)
	users := map[string]string{}
	for _, d := range all {
		users[d.DeviceID] = d.User
	}
	d := copyData{DeviceID: id}
	for _, t := range targets {
		res := copyResult{DeviceID: t, User: users[t]}
		tctx, tcancel := u.ctx(r)
		out, err := u.cfg.Client.ReplaceDirectory(tctx, t, contacts, "", false)
		tcancel()
		if err != nil {
			res.Err = describe(err)
		} else {
			res.Result = out
		}
		d.Results = append(d.Results, res)
	}
	u.render(w, "copy.html", page{Title: "Copy directory", Nav: "devices", CSRF: csrf(r), Data: d})
}

func copyBody(w http.ResponseWriter, resp *http.Response) {
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
