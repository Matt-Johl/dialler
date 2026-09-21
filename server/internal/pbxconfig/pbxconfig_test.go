package pbxconfig

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateNormalisesAndRefuses(t *testing.T) {
	s := Settings{Mode: ModeRegister, Host: " cucm.example ", Transport: "TLS", Codecs: "G722, pcmu", SRTP: "SDES"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Host != "cucm.example" || s.Port != 5061 || s.Transport != "tls" || s.Codecs != "g722,pcmu" || s.SRTP != "sdes" || s.ExpirySeconds != 300 {
		t.Fatalf("normalised: %+v", s)
	}
	for _, bad := range []Settings{
		{Mode: "bridge", Host: "x"},
		{Mode: ModePeer},
		{Mode: ModePeer, Host: "x", Transport: "sctp"},
		{Mode: ModePeer, Host: "x", Port: 70000},
		{Mode: ModeRegister, Host: "x", ExpirySeconds: 5},
		{Mode: ModePeer, Host: "x", Codecs: "mp3"},
		{Mode: ModePeer, Host: "x", SRTP: "dtls"},
		{Mode: ModePeer, Host: "x", QualifySeconds: -1},
		{Mode: ModePeer, Host: "x", Transport: "udp", TLSCA: "ca.pem"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestStoreAndHandler(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pbx.json")
	s, _ := Open(path)
	if s.Get() != nil {
		t.Fatal("nothing saved yet")
	}
	var changed []Settings
	h := Handler(s, "admin", func(st Settings) { changed = append(changed, st) })
	do := func(method, body string, auth bool) (int, string) {
		req := httptest.NewRequest(method, "/v1/admin/pbx", strings.NewReader(body))
		if auth {
			req.Header.Set("Authorization", "Bearer admin")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := do("GET", "", false); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := do("GET", "", true); code != 404 {
		t.Fatalf("unset: %d", code)
	}
	if code, body := do("PUT", `{"mode":"peer"}`, true); code != 400 || !strings.Contains(body, "host is required") {
		t.Fatalf("invalid: %d %s", code, body)
	}
	code, body := do("PUT", `{"mode":"register","host":"cucm.example","transport":"tcp","expiry_seconds":600}`, true)
	var st Settings
	_ = json.Unmarshal([]byte(body), &st)
	if code != 200 || st.Version != 1 || st.Port != 5060 || st.Mode != ModeRegister {
		t.Fatalf("set: %d %s", code, body)
	}
	if code, _ := do("PUT", `{"mode":"peer","host":"asterisk"}`, true); code != 200 {
		t.Fatalf("second set: %d", code)
	}
	if len(changed) != 2 || changed[1].Version != 2 {
		t.Fatalf("onChange: %+v", changed)
	}
	again, _ := Open(path)
	if got := again.Get(); got == nil || got.Version != 2 || got.Host != "asterisk" {
		t.Fatalf("persisted: %+v", got)
	}
}
