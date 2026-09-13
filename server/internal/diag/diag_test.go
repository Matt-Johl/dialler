package diag

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func passAuth(h http.Handler) http.Handler { return h }

// No listener (the sandbox forbids binding): the handler is driven directly.
func post(h http.Handler, kind, name, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/diag", strings.NewReader(body))
	req.Header.Set("X-Device-ID", "dev-a")
	if kind != "" {
		req.Header.Set("X-Diag-Kind", kind)
	}
	if name != "" {
		req.Header.Set("X-Diag-Name", name)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestUploadIsStoredPerDevice(t *testing.T) {
	dir := t.TempDir()
	h := Handler(dir, passAuth, nil)

	if rec := post(h, "metrickit", "../evil name", "{\"crash\":true}"); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "dev-a", "*-metrickit-evilname.json"))
	if len(files) != 1 {
		t.Fatalf("stored files: %v", files)
	}
	if b, _ := os.ReadFile(files[0]); string(b) != "{\"crash\":true}" {
		t.Fatalf("content %q", b)
	}
	if rec := post(h, "app-log", "", "line\n"); rec.Code != http.StatusNoContent {
		t.Fatalf("log status %d", rec.Code)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "dev-a", "*-app-log.log")); len(files) != 1 {
		t.Fatalf("log files: %v", files)
	}

	if rec := post(h, "", "", "x"); rec.Code != http.StatusBadRequest {
		t.Fatalf("no kind: status %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/diag", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: status %d", rec.Code)
	}
}
