package admin

import (
	"net/http/httptest"
	"testing"
)

func TestIfMatch(t *testing.T) {
	cases := []struct {
		header string
		want   string
		sent   bool
	}{
		{"", "", false},
		{"*", "", false},
		{`"41"`, "41", true},
		{"41", "41", true},
		{`W/"41"`, "41", true},
		{`  "2026-09-25T10:00:00Z" `, "2026-09-25T10:00:00Z", true},
	}
	for _, c := range cases {
		req := httptest.NewRequest("PUT", "/", nil)
		if c.header != "" {
			req.Header.Set("If-Match", c.header)
		}
		got, sent := IfMatch(req)
		if got != c.want || sent != c.sent {
			t.Fatalf("%q: got %q %v, want %q %v", c.header, got, sent, c.want, c.sent)
		}
	}
	req := httptest.NewRequest("PUT", "/", nil)
	req.Header.Set("If-Match", `"-1"`)
	rec := httptest.NewRecorder()
	if _, _, done := IfMatchInt(rec, req); !done || rec.Code != 400 {
		t.Fatalf("negative version: done=%v %d", done, rec.Code)
	}
	req.Header.Set("If-Match", `"7"`)
	if v, sent, done := IfMatchInt(httptest.NewRecorder(), req); done || !sent || *v != 7 {
		t.Fatal("version 7")
	}
	req.Header.Set("If-Match", `"not a time"`)
	rec = httptest.NewRecorder()
	if _, _, done := IfMatchTime(rec, req); !done || rec.Code != 400 {
		t.Fatalf("bad time: done=%v %d", done, rec.Code)
	}
	req.Header.Set("If-Match", `"2026-09-25T10:00:00.5Z"`)
	if tm, sent, done := IfMatchTime(httptest.NewRecorder(), req); done || !sent || tm.Nanosecond() != 500000000 {
		t.Fatal("time with fraction")
	}
}
