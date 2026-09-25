package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// No listener (the sandbox forbids binding): the mux is driven directly.
func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func anyDevice(string) bool { return true }

// write puts a file under dir/device with the given mtime.
func write(t *testing.T, dir, device, name, body string, at time.Time) string {
	t.Helper()
	d := filepath.Join(dir, device)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, at, at); err != nil {
		t.Fatal(err)
	}
	return p
}

func envelope(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, want application/json", ct)
	}
	var body struct{ Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not an envelope: %s", rec.Body)
	}
	if body.Error != code {
		t.Fatalf("error %q, want %q", body.Error, code)
	}
}

func TestListNewestFirstWithFields(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 25, 8, 55, 12, 0, time.UTC)
	write(t, dir, "dev-a", "20260925T085512.000Z-applog-x.log", "old", base)
	write(t, dir, "dev-a", "20260925T090000.000Z-metrickit.json", "{}", base.Add(5*time.Minute))
	write(t, dir, "dev-a", "20260925T091500.000Z-ips-crash.ips", "newest!", base.Add(20*time.Minute))
	// Neither a directory nor a symlink is an upload.
	if err := os.Mkdir(filepath.Join(dir, "dev-a", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "dev-a", "20260925T085512.000Z-applog-x.log"), filepath.Join(dir, "dev-a", "link.log")); err != nil {
		t.Fatal(err)
	}

	rec := do(AdminHandler(dir, anyDevice), http.MethodGet, "/v1/admin/devices/dev-a/diag")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got []Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Name: "20260925T091500.000Z-ips-crash.ips", Kind: "ips", Size: 7, At: "2026-09-25T09:15:12Z"},
		{Name: "20260925T090000.000Z-metrickit.json", Kind: "metrickit", Size: 2, At: "2026-09-25T09:00:12Z"},
		{Name: "20260925T085512.000Z-applog-x.log", Kind: "applog", Size: 3, At: "2026-09-25T08:55:12Z"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestListEmptyIsAnArray(t *testing.T) {
	rec := do(AdminHandler(t.TempDir(), anyDevice), http.MethodGet, "/v1/admin/devices/dev-a/diag")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if s := strings.TrimSpace(rec.Body.String()); s != "[]" {
		t.Fatalf("body %q, want []", s)
	}
}

func TestListCapped(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < MaxList+5; i++ {
		at := base.Add(time.Duration(i) * time.Second)
		write(t, dir, "dev-a", at.Format(timeLayout)+"-applog.log", "x", at)
	}
	var got []Entry
	rec := do(AdminHandler(dir, anyDevice), http.MethodGet, "/v1/admin/devices/dev-a/diag")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != MaxList {
		t.Fatalf("%d entries, want %d", len(got), MaxList)
	}
	if got[0].Name != base.Add(time.Duration(MaxList+4)*time.Second).Format(timeLayout)+"-applog.log" {
		t.Fatalf("first entry %q is not the newest", got[0].Name)
	}
}

func TestKind(t *testing.T) {
	cases := map[string]string{
		"20260925T085512.000Z-applog-x.log":         "applog",
		"20260925T085512.000Z-applog.log":           "applog",
		"20260925T085512.000Z-metrickit.json":       "metrickit",
		"20260925T085512.000Z-ips-Dialler.ips":      "ips",
		"20260925T085512.000Z-extensionlog-a-b.log": "extensionlog",
		"20260925T085512.000Z-.log":                 "",
		"20260925T085512.000Z":                      "",
		"applog.log":                                "",
		"notatime-applog.log":                       "",
		"":                                          "",
	}
	for name, want := range cases {
		if got := Kind(name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestDownloadContentTypeAndDisposition(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cases := map[string]string{
		"20260925T085512.000Z-applog.log":       "text/plain; charset=utf-8",
		"20260925T085512.000Z-other.txt":        "text/plain; charset=utf-8",
		"20260925T085512.000Z-ips-crash.ips":    "text/plain; charset=utf-8",
		"20260925T085512.000Z-metrickit.json":   "application/json",
		"20260925T085512.000Z-blob.bin":         "application/octet-stream",
		"20260925T085512.000Z-noext":            "application/octet-stream",
		"20260925T085512.000Z-upper.LOG":        "text/plain; charset=utf-8",
		"20260925T085512.000Z-tricky.json.html": "application/octet-stream",
	}
	h := AdminHandler(dir, anyDevice)
	for name, ct := range cases {
		write(t, dir, "dev-a", name, "body of "+name, now)
		rec := do(h, http.MethodGet, "/v1/admin/devices/dev-a/diag/"+name)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", name, rec.Code, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); got != ct {
			t.Errorf("%s: Content-Type %q, want %q", name, got, ct)
		}
		if got, want := rec.Header().Get("Content-Disposition"), `attachment; filename="`+name+`"`; got != want {
			t.Errorf("%s: Content-Disposition %q, want %q", name, got, want)
		}
		if rec.Body.String() != "body of "+name {
			t.Errorf("%s: body %q", name, rec.Body)
		}
	}
}

func TestDownloadAbsentIs404(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dev-a", "20260925T085512.000Z-applog.log", "x", time.Now())
	rec := do(AdminHandler(dir, anyDevice), http.MethodGet, "/v1/admin/devices/dev-a/diag/20260925T085513.000Z-applog.log")
	envelope(t, rec, http.StatusNotFound, "not_found")
	// A directory is not a file either.
	if err := os.Mkdir(filepath.Join(dir, "dev-a", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	envelope(t, do(AdminHandler(dir, anyDevice), http.MethodGet, "/v1/admin/devices/dev-a/diag/sub"), http.StatusNotFound, "not_found")
}

func TestDownloadRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.log")
	if err := os.WriteFile(outside, []byte("not for you"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "dev-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "dev-a", "20260925T085512.000Z-applog.log")); err != nil {
		t.Fatal(err)
	}
	h := AdminHandler(dir, anyDevice)
	rec := do(h, http.MethodGet, "/v1/admin/devices/dev-a/diag/20260925T085512.000Z-applog.log")
	envelope(t, rec, http.StatusNotFound, "not_found")
	if bytes.Contains(rec.Body.Bytes(), []byte("not for you")) {
		t.Fatal("the symlink was followed")
	}
	// Deleting through it is refused too, and the target survives.
	rec = do(h, http.MethodDelete, "/v1/admin/devices/dev-a/diag/20260925T085512.000Z-applog.log")
	envelope(t, rec, http.StatusNotFound, "not_found")
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("target: %v", err)
	}
	// And the listing does not show it.
	rec = do(h, http.MethodGet, "/v1/admin/devices/dev-a/diag")
	if s := strings.TrimSpace(rec.Body.String()); s != "[]" {
		t.Fatalf("listing %s, want []", s)
	}
}

// TestBadNamesNeverTouchDisk points dir at a path under a regular file, so
// any filesystem access would fail with ENOTDIR and answer 500, not 404.
func TestBadNamesNeverTouchDisk(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "diag")
	var asked []string
	h := AdminHandler(dir, func(id string) bool { asked = append(asked, id); return true })

	// A literal "." or ".." never reaches a handler: http.ServeMux cleans
	// the path and answers 301 before routing. The escaped forms do reach
	// {name}, unescaped, and are what ValidDiagName refuses.
	names := []string{
		"..%2F..%2Fetc%2Fpasswd", "%2E%2E%2Fpbx.key", "%2E%2E", "%2E", "a%2Fb", "bad%20name",
		"nul%00.log", "~x.log", "ünïcode.log", strings.Repeat("a", 161) + ".log",
	}
	for _, n := range names {
		for _, m := range []string{http.MethodGet, http.MethodDelete} {
			rec := do(h, m, "/v1/admin/devices/dev-a/diag/"+n)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %q: status %d, want 404: %s", m, n, rec.Code, rec.Body)
			}
		}
	}
	// A sane name, on the other hand, does reach the disk and fails there.
	rec := do(h, http.MethodGet, "/v1/admin/devices/dev-a/diag/20260925T085512.000Z-applog.log")
	envelope(t, rec, http.StatusInternalServerError, "store")
	if len(asked) == 0 {
		t.Fatal("knownDevice was never consulted")
	}
}

func TestBadDeviceIDsNeverTouchDiskOrTheDeviceSet(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var asked []string
	h := AdminHandler(filepath.Join(blocker, "diag"), func(id string) bool { asked = append(asked, id); return true })
	// As above, a literal ".." is the mux's 301; the escaped one reaches {id}.
	for _, id := range []string{"-dev", "%2E%2E", "dev%2Fa", "dev.a", strings.Repeat("d", 65), "dév"} {
		for _, tgt := range []string{"/v1/admin/devices/" + id + "/diag", "/v1/admin/devices/" + id + "/diag/x.log"} {
			for _, m := range []string{http.MethodGet, http.MethodDelete} {
				rec := do(h, m, tgt)
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s %s: status %d, want 404: %s", m, tgt, rec.Code, rec.Body)
				}
			}
		}
	}
	if len(asked) != 0 {
		t.Fatalf("knownDevice consulted for ids that fail the grammar: %v", asked)
	}
}

func TestUnknownDeviceIs404(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dev-b", "20260925T085512.000Z-applog.log", "x", time.Now())
	var asked []string
	h := AdminHandler(dir, func(id string) bool { asked = append(asked, id); return id == "dev-a" })
	for _, tgt := range []string{"/v1/admin/devices/dev-b/diag", "/v1/admin/devices/dev-b/diag/20260925T085512.000Z-applog.log"} {
		for _, m := range []string{http.MethodGet, http.MethodDelete} {
			envelope(t, do(h, m, tgt), http.StatusNotFound, "not_found")
		}
	}
	if len(asked) != 4 || asked[0] != "dev-b" {
		t.Fatalf("knownDevice calls: %v", asked)
	}
	// The files a stranger could not reach are still there.
	if _, err := os.Stat(filepath.Join(dir, "dev-b", "20260925T085512.000Z-applog.log")); err != nil {
		t.Fatal(err)
	}
	// A nil knownDevice trusts the grammar alone.
	if rec := do(AdminHandler(dir, nil), http.MethodGet, "/v1/admin/devices/dev-b/diag/20260925T085512.000Z-applog.log"); rec.Code != http.StatusOK {
		t.Fatalf("nil knownDevice: status %d", rec.Code)
	}
}

func TestDeleteOne(t *testing.T) {
	dir := t.TempDir()
	keep := write(t, dir, "dev-a", "20260925T085512.000Z-applog.log", "x", time.Now())
	gone := write(t, dir, "dev-a", "20260925T085513.000Z-applog.log", "y", time.Now())
	h := AdminHandler(dir, anyDevice)
	rec := do(h, http.MethodDelete, "/v1/admin/devices/dev-a/diag/20260925T085513.000Z-applog.log")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if _, err := os.Lstat(gone); !os.IsNotExist(err) {
		t.Fatalf("deleted file: %v", err)
	}
	if _, err := os.Lstat(keep); err != nil {
		t.Fatalf("other file: %v", err)
	}
	// Again: 404.
	envelope(t, do(h, http.MethodDelete, "/v1/admin/devices/dev-a/diag/20260925T085513.000Z-applog.log"), http.StatusNotFound, "not_found")
}

func TestDeleteAll(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dev-a", "20260925T085512.000Z-applog.log", "x", time.Now())
	write(t, dir, "dev-a", "20260925T085513.000Z-applog.log", "y", time.Now())
	other := write(t, dir, "dev-b", "20260925T085512.000Z-applog.log", "z", time.Now())
	h := AdminHandler(dir, anyDevice)
	rec := do(h, http.MethodDelete, "/v1/admin/devices/dev-a/diag")
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if _, err := os.Lstat(filepath.Join(dir, "dev-a")); !os.IsNotExist(err) {
		t.Fatalf("device directory: %v", err)
	}
	if _, err := os.Lstat(other); err != nil {
		t.Fatalf("other device's file: %v", err)
	}
	// A device with nothing is 204 too.
	if rec := do(h, http.MethodDelete, "/v1/admin/devices/dev-a/diag"); rec.Code != http.StatusNoContent {
		t.Fatalf("second delete: status %d: %s", rec.Code, rec.Body)
	}
	if rec := do(h, http.MethodDelete, "/v1/admin/devices/dev-c/diag"); rec.Code != http.StatusNoContent {
		t.Fatalf("never-uploaded device: status %d: %s", rec.Code, rec.Body)
	}
}

func TestSweepRemovesOnlyOldFilesAndYields(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	retain := 30 * 24 * time.Hour
	oldA := write(t, dir, "dev-a", "20260801T000000.000Z-applog.log", "x", now.Add(-31*24*time.Hour))
	newA := write(t, dir, "dev-a", "20260924T000000.000Z-applog.log", "x", now.Add(-24*time.Hour))
	edgeA := write(t, dir, "dev-a", "20260826T120000.000Z-applog.log", "x", now.Add(-retain))
	oldB := write(t, dir, "dev-b", "20260701T000000.000Z-metrickit.json", "{}", now.Add(-90*24*time.Hour))
	// A symlink to an old file outside dir is not an upload and is left alone.
	outside := filepath.Join(t.TempDir(), "old.log")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(outside, now.Add(-400*24*time.Hour), now.Add(-400*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "dev-b", "link.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// A stray file at the top level is not a device directory.
	stray := filepath.Join(dir, "stray.log")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stray, now.Add(-400*24*time.Hour), now.Add(-400*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	yields := 0
	yield = func() { yields++ }
	t.Cleanup(func() { yield = func() {} })

	n, err := Sweep(dir, retain, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("removed %d, want 2", n)
	}
	for _, p := range []string{oldA, oldB} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be gone: %v", p, err)
		}
	}
	for _, p := range []string{newA, edgeA, link, stray, outside} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s should remain: %v", p, err)
		}
	}
	// Four regular files were visited, one yield after each.
	if yields != 4 {
		t.Fatalf("yielded %d times, want 4", yields)
	}
}

func TestSweepRetainZeroKeepsAll(t *testing.T) {
	dir := t.TempDir()
	old := write(t, dir, "dev-a", "20200101T000000.000Z-applog.log", "x", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, retain := range []time.Duration{0, -time.Hour} {
		n, err := Sweep(dir, retain, time.Now())
		if err != nil || n != 0 {
			t.Fatalf("retain %v: removed %d, err %v", retain, n, err)
		}
		if _, err := os.Lstat(old); err != nil {
			t.Fatalf("retain %v: %v", retain, err)
		}
	}
}

func TestSweepMissingDirIsEmpty(t *testing.T) {
	n, err := Sweep(filepath.Join(t.TempDir(), "nope"), time.Hour, time.Now())
	if err != nil || n != 0 {
		t.Fatalf("removed %d, err %v", n, err)
	}
}

func TestSweepContinuesPastErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	past := time.Now().Add(-48 * time.Hour)
	locked := write(t, dir, "dev-a", "20260101T000000.000Z-applog.log", "x", past)
	free := write(t, dir, "dev-b", "20260101T000000.000Z-applog.log", "x", past)
	// dev-a's directory cannot be written, so its file cannot be unlinked.
	if err := os.Chmod(filepath.Join(dir, "dev-a"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "dev-a"), 0o755) })

	n, err := Sweep(dir, 24*time.Hour, time.Now())
	if err == nil {
		t.Fatal("expected an error for the locked directory")
	}
	if n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Lstat(locked); err != nil {
		t.Fatalf("locked file: %v", err)
	}
	if _, err := os.Lstat(free); !os.IsNotExist(err) {
		t.Fatalf("free file should be gone: %v", err)
	}
}

func TestRunSweeperSweepsAtStartAndLogsTheCount(t *testing.T) {
	dir := t.TempDir()
	old := write(t, dir, "dev-a", "20200101T000000.000Z-applog.log", "x", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the start sweep still runs; the loop then returns at once
	RunSweeper(ctx, dir, 24*time.Hour, time.Hour, log)
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("old file: %v", err)
	}
	if s := buf.String(); !strings.Contains(s, "diag: swept") || !strings.Contains(s, "removed=1") {
		t.Fatalf("log: %q", s)
	}
	// Nothing to remove: nothing logged.
	buf.Reset()
	RunSweeper(ctx, dir, 24*time.Hour, time.Hour, log)
	if buf.Len() != 0 {
		t.Fatalf("log after a no-op sweep: %q", buf.String())
	}
}
