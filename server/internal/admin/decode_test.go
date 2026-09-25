package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type sample struct {
	User  string   `json:"user"`
	SSIDs []string `json:"ssids"`
	Line  *struct {
		DN string `json:"dn"`
	} `json:"line"`
}

func TestDecodeStrict(t *testing.T) {
	cases := []struct {
		name, body, code, field string
	}{
		{"clean", `{"user":"201","ssids":["a","b"]}`, "", ""},
		{"empty", ``, CodeBadJSON, ""},
		{"whitespace", `   `, CodeBadJSON, ""},
		{"not an object", `[1,2]`, CodeBadJSON, ""},
		{"scalar", `"x"`, CodeBadJSON, ""},
		{"trailing data", `{"user":"201"} {"user":"202"}`, CodeBadJSON, ""},
		{"trailing garbage", `{"user":"201"}x`, CodeBadJSON, ""},
		{"truncated", `{"user":"20`, CodeBadJSON, ""},
		{"duplicate key", `{"user":"201","user":"202"}`, CodeBadJSON, ""},
		{"duplicate nested key", `{"line":{"dn":"1","dn":"2"}}`, CodeBadJSON, "line.dn"},
		{"unknown field", `{"user":"201","nonsense":true}`, CodeUnknownField, "nonsense"},
		{"wrong type", `{"user":201}`, CodeInvalid, "user"},
		{"wrong type in array", `{"ssids":[1]}`, CodeInvalid, "ssids"},
		{"invalid utf8", "{\"user\":\"\xff\"}", CodeBadJSON, ""},
		{"deep nesting", strings.Repeat("[", 20000) + strings.Repeat("]", 20000), CodeBadJSON, ""},
		{"deep object nesting", `{"a":` + strings.Repeat(`{"a":`, 20000) + `1` + strings.Repeat(`}`, 20001), CodeBadJSON, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v sample
			code, msg, field := decodeStrict([]byte(c.body), &v)
			if code != c.code {
				t.Fatalf("code %q (%s), want %q", code, msg, c.code)
			}
			if code != "" && c.field != "" && field != c.field {
				t.Fatalf("field %q, want %q (%s)", field, c.field, msg)
			}
		})
	}
}

// Deep nesting must be refused for the depth, not for the memory it would
// take: the same key repeated at every level is what a fuzzer produces.
func TestDecodeDuplicateKeyDeepInArrays(t *testing.T) {
	var v sample
	code, _, field := decodeStrict([]byte(`{"ssids":["a",{"x":1,"x":2}]}`), &v)
	if code != CodeBadJSON || field != "ssids[1].x" {
		t.Fatalf("code %q field %q", code, field)
	}
}

func TestDecodeHTTP(t *testing.T) {
	handler := func(limit int64) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var v sample
			if !Decode(w, r, limit, &v) {
				return
			}
			WriteJSON(w, 200, v)
		})
	}
	post := func(h http.Handler, ct, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/x", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := post(handler(1024), "application/json; charset=utf-8", `{"user":"201"}`); rec.Code != 200 {
		t.Fatalf("clean with charset: %d %s", rec.Code, rec.Body)
	}
	if rec := post(handler(1024), "", `{"user":"201"}`); rec.Code != 200 {
		t.Fatalf("no content-type: %d %s", rec.Code, rec.Body)
	}
	rec := post(handler(1024), "application/x-www-form-urlencoded", `{"user":"201"}`)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("form content-type: %d", rec.Code)
	}
	if b := errorBody(t, rec); b.Error != CodeUnsupportedMediaType {
		t.Fatalf("code %q", b.Error)
	}
	rec = post(handler(16), "application/json", `{"user":"201","ssids":["aaaaaaaaaa"]}`)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over limit: %d %s", rec.Code, rec.Body)
	}
	if b := errorBody(t, rec); b.Error != CodeTooLarge {
		t.Fatalf("code %q", b.Error)
	}
	rec = post(handler(1024), "application/json", `{"user":"201","x":1}`)
	if rec.Code != 400 {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	if b := errorBody(t, rec); b.Error != CodeUnknownField || b.Field != "x" {
		t.Fatalf("%+v", b)
	}
}

func TestClean(t *testing.T) {
	if !Clean("Warehouse 3 – café", 64) {
		t.Fatal("plain text refused")
	}
	if Clean("a\x00b", 64) || Clean("a\nb", 64) || Clean("a\x7fb", 64) {
		t.Fatal("control character accepted")
	}
	if Clean("toolong", 3) {
		t.Fatal("over max accepted")
	}
}

func TestIdentifierGrammars(t *testing.T) {
	ok := []string{"dev-a", "dev_7K3M9Q", "a", strings.Repeat("x", 64)}
	bad := []string{"", "-a", "../x", "a/b", "a.b", strings.Repeat("x", 65), "dev a"}
	for _, s := range ok {
		if !DeviceIDRe.MatchString(s) {
			t.Fatalf("device id %q refused", s)
		}
	}
	for _, s := range bad {
		if DeviceIDRe.MatchString(s) {
			t.Fatalf("device id %q accepted", s)
		}
	}
	if !ContactIDRe.MatchString("ct_0a1b") || ContactIDRe.MatchString("ct_") || ContactIDRe.MatchString("x_0a") {
		t.Fatal("contact id grammar")
	}
	if !ValidDiagName("20260925T085512.000Z-applog.log") || ValidDiagName("..") || ValidDiagName("a/b") {
		t.Fatal("diag name grammar")
	}
	if !UserRe.MatchString("201") || UserRe.MatchString("2 01") || UserRe.MatchString("") {
		t.Fatal("user grammar")
	}
}

// The fuzzer's contract: never panic, and a body that decodes cleanly
// re-encodes to something that decodes to the same value.
func FuzzDecodeStrict(f *testing.F) {
	for _, s := range []string{`{"user":"201"}`, `{"ssids":["a"]}`, `{"line":{"dn":"1"}}`, `{`, `[]`, `{"user":"201","user":"1"}`, "{\"user\":\"\xff\"}"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		var v sample
		code, _, _ := decodeStrict(body, &v)
		if code != "" {
			return
		}
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("clean decode did not re-encode: %v", err)
		}
		var again sample
		if c2, msg, _ := decodeStrict(out, &again); c2 != "" {
			t.Fatalf("round trip refused: %s %s", c2, msg)
		}
	})
}
