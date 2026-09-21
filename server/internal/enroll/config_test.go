package enroll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Server-managed device settings (SPEC §6 item 8b): set by the admin per
// device, versioned, persisted, and absent until set.
func TestSetConfigVersionsPersistsAndIsolates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	s, _ := Open(path)
	s.Create("dev-a", "201", "")
	s.Create("dev-b", "202", "")
	if s.Config("dev-a") != nil {
		t.Fatal("no settings until an admin sets them")
	}
	if d := s.Devices()[0]; d.Config != nil {
		t.Fatalf("device view: %+v", d)
	}

	cfg, err := s.SetConfig("dev-a", []string{" Office ", "", "Office-5G", "Office"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 || !reflect.DeepEqual(cfg.SSIDs, []string{"Office", "Office-5G"}) {
		t.Fatalf("first set: %+v (trimmed, empties and repeats dropped, order kept)", cfg)
	}
	// The same list again still bumps: an admin's "apply" always reaches
	// the phone, which decides for itself whether anything changed.
	cfg, _ = s.SetConfig("dev-a", []string{"Office", "Office-5G"})
	if cfg.Version != 2 {
		t.Fatalf("re-set: %+v", cfg)
	}
	// An empty list is a setting too (no background wakes), not "unset".
	cfg, _ = s.SetConfig("dev-a", []string{})
	if cfg.Version != 3 || len(cfg.SSIDs) != 0 || s.Config("dev-a") == nil {
		t.Fatalf("empty set: %+v", cfg)
	}
	if _, err := s.SetConfig("nobody", []string{"x"}); err != ErrInvalid {
		t.Fatalf("unknown device: %v", err)
	}
	if s.Config("dev-b") != nil {
		t.Fatal("dev-b was touched")
	}

	s2, _ := Open(path)
	if got := s2.Config("dev-a"); got == nil || got.Version != 3 {
		t.Fatalf("persisted: %+v", got)
	}
	if d := s2.Devices()[0]; d.Config == nil || d.Config.Version != 3 {
		t.Fatalf("device view after reload: %+v", d)
	}
}

func TestAdminConfigRoutes(t *testing.T) {
	s, _ := Open("")
	s.Create("dev-a", "201", "")
	var pushed []DeviceConfig
	h := NewAdminHandler(s, "admin", Link{}, Hooks{OnConfig: func(d string, c DeviceConfig) {
		if d != "dev-a" {
			t.Fatalf("pushed to %s", d)
		}
		pushed = append(pushed, c)
	}})
	do := func(method, path, body string) (*http.Response, []byte) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Result(), rec.Body.Bytes()
	}
	if resp, _ := do("GET", "/v1/admin/devices/dev-a/config", ""); resp.StatusCode != 404 {
		t.Fatalf("unset config: %d", resp.StatusCode)
	}
	if resp, _ := do("PUT", "/v1/admin/devices/dev-zz/config", `{"ssids":["x"]}`); resp.StatusCode != 404 {
		t.Fatalf("unknown device: %d", resp.StatusCode)
	}
	if resp, _ := do("PUT", "/v1/admin/devices/dev-a/config", `{"nope":1}`); resp.StatusCode != 400 {
		t.Fatalf("bad body: %d", resp.StatusCode)
	}
	resp, body := do("PUT", "/v1/admin/devices/dev-a/config", `{"ssids":["Office","Office-5G"]}`)
	var cfg DeviceConfig
	_ = json.Unmarshal(body, &cfg)
	if resp.StatusCode != 200 || cfg.Version != 1 || len(cfg.SSIDs) != 2 {
		t.Fatalf("set: %d %s", resp.StatusCode, body)
	}
	if resp, _ := do("POST", "/v1/admin/devices/dev-a/config", `{"ssids":[]}`); resp.StatusCode != 200 {
		t.Fatalf("POST alias: %d", resp.StatusCode)
	}
	resp, body = do("GET", "/v1/admin/devices/dev-a/config", "")
	_ = json.Unmarshal(body, &cfg)
	if resp.StatusCode != 200 || cfg.Version != 2 || len(cfg.SSIDs) != 0 {
		t.Fatalf("get: %d %s", resp.StatusCode, body)
	}
	if len(pushed) != 2 || pushed[0].Version != 1 || pushed[1].Version != 2 {
		t.Fatalf("OnConfig: %+v", pushed)
	}
}
