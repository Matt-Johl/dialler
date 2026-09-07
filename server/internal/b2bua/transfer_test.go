package b2bua

import (
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestReferTarget(t *testing.T) {
	cases := map[string]string{
		"sip:100@dialler":               "sip:100@dialler",
		"sip:202@dialler;transport=tls": "sip:202@dialler",
		"sip:echo@dialler":              "sip:echo@dialler",
		"sip:0123@pbx.example.com":      "sip:0123@pbx.example.com",
	}
	for in, want := range cases {
		var u sip.Uri
		if err := sip.ParseUri(in, &u); err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got := referTarget(u); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	if got := referTarget(sip.Uri{User: "202"}); got != "202" {
		t.Errorf("bare user → %s", got)
	}
}

func TestReferNotify(t *testing.T) {
	var remote sip.Uri
	if err := sip.ParseUri("sip:201-abc@10.18.0.111:53414;transport=tls", &remote); err != nil {
		t.Fatal(err)
	}
	req := referNotify(remote, 200, "OK")
	if req.Method != sip.NOTIFY {
		t.Fatalf("method %s", req.Method)
	}
	if req.Recipient.String() != remote.String() {
		t.Errorf("request URI %s, want %s", req.Recipient.String(), remote.String())
	}
	if h := req.GetHeader("Event"); h == nil || h.Value() != "refer" {
		t.Errorf("Event header: %v", h)
	}
	if h := req.GetHeader("Subscription-State"); h == nil || !strings.HasPrefix(h.Value(), "terminated") {
		t.Errorf("Subscription-State: %v", h)
	}
	if h := req.GetHeader("Content-Type"); h == nil || !strings.HasPrefix(h.Value(), "message/sipfrag") {
		t.Errorf("Content-Type: %v", h)
	}
	if string(req.Body()) != "SIP/2.0 200 OK" {
		t.Errorf("body %q", req.Body())
	}
	if string(referNotify(remote, 404, "Not Found").Body()) != "SIP/2.0 404 Not Found" {
		t.Error("failure sipfrag")
	}
}

func TestReferErrorsCarryStatus(t *testing.T) {
	if (&sipgo404{}).Error() != "404 Not Found" || (&sipgo488{}).Error() != "488 Not Acceptable Here" {
		t.Error("status text")
	}
}
