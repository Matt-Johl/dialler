package pbxline

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

// These are the shapes an exchange puts on the wire. Asterisk can be made to
// send some of them; CUCM's are the ones we cannot stage at all until a
// cluster exists (SPEC §6 item 3c), so they are pinned here instead — the
// challenge parsing and the response arithmetic are pure, and this is the
// part that has to be right on the first call to a live CUCM.

func challengeResponse(t *testing.T, code int, headerName, value string) *sip.Response {
	t.Helper()
	res := sip.NewResponse(code, "Unauthorized")
	res.AppendHeader(sip.NewHeader(headerName, value))
	return res
}

// recompute derives the digest response independently of the library, the
// way the exchange will when it checks us.
func recompute(t *testing.T, cred *digest.Credentials, method, password string) string {
	t.Helper()
	h := func(format string, args ...any) string {
		sum := md5.Sum([]byte(fmt.Sprintf(format, args...)))
		return hex.EncodeToString(sum[:])
	}
	a1 := h("%s:%s:%s", cred.Username, cred.Realm, password)
	a2 := h("%s:%s", method, cred.URI)
	if cred.QOP == "" {
		return h("%s:%s:%s", a1, cred.Nonce, a2)
	}
	return h("%s:%s:%08x:%s:%s:%s", a1, cred.Nonce, cred.Nc, cred.Cnonce, cred.QOP, a2)
}

func TestAnswersTheChallengeShapesAnExchangeSends(t *testing.T) {
	const (
		user     = "matt"
		password = "hunter2"
		uri      = "sip:cucm.example.com"
	)
	cases := []struct {
		name   string
		code   int
		header string
		value  string
		// wantHeader is where our answer belongs.
		wantHeader string
		wantQOP    string
		wantRealm  string
	}{
		{
			// CUCM's line-side challenge: MD5, qop=auth, its own realm —
			// which is not the host we sent the REGISTER to, and must be
			// taken from the challenge rather than assumed.
			name: "CUCM line, qop auth", code: 401, header: "WWW-Authenticate",
			value:      `Digest realm="ccmsipline", nonce="YjM0NWY2Nzg5MGFiY2RlZg==", algorithm=MD5, qop="auth"`,
			wantHeader: "Authorization", wantQOP: "auth", wantRealm: "ccmsipline",
		},
		{
			// RFC 2069-era: no qop at all. Asterisk sends this.
			name: "no qop", code: 401, header: "WWW-Authenticate",
			value:      `Digest realm="asterisk", nonce="1a2b3c4d"`,
			wantHeader: "Authorization", wantQOP: "", wantRealm: "asterisk",
		},
		{
			// A proxy in front of the exchange challenges with 407, and the
			// answer belongs in a different header.
			name: "proxy", code: 407, header: "Proxy-Authenticate",
			value:      `Digest realm="edge", nonce="ZZ==", algorithm=MD5, qop="auth"`,
			wantHeader: "Proxy-Authorization", wantQOP: "auth", wantRealm: "edge",
		},
		{
			// Opaque must be echoed back untouched.
			name: "with opaque", code: 401, header: "WWW-Authenticate",
			value:      `Digest realm="ccmsipline", nonce="n1", opaque="o1", algorithm=MD5, qop="auth"`,
			wantHeader: "Authorization", wantQOP: "auth", wantRealm: "ccmsipline",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := challengeResponse(t, tc.code, tc.header, tc.value)
			chal, hdr, err := challenge(res)
			if err != nil {
				t.Fatalf("challenge: %v", err)
			}
			if hdr != tc.wantHeader {
				t.Errorf("answer header = %q, want %q", hdr, tc.wantHeader)
			}
			if chal.Realm != tc.wantRealm {
				t.Errorf("realm = %q, want %q", chal.Realm, tc.wantRealm)
			}

			value, err := authorization(chal, "REGISTER", uri, user, password)
			if err != nil {
				t.Fatalf("authorization: %v", err)
			}
			cred, err := digest.ParseCredentials(value)
			if err != nil {
				t.Fatalf("our own credentials do not parse: %v (%s)", err, value)
			}
			if cred.Username != user || cred.Realm != tc.wantRealm || cred.Nonce != chal.Nonce || cred.URI != uri {
				t.Errorf("credentials carry the wrong identity: %+v", cred)
			}
			if cred.QOP != tc.wantQOP {
				t.Errorf("qop = %q, want %q", cred.QOP, tc.wantQOP)
			}
			if chal.Opaque != "" && cred.Opaque != chal.Opaque {
				t.Errorf("opaque not echoed: %q", cred.Opaque)
			}
			if tc.wantQOP == "auth" {
				if cred.Cnonce == "" {
					t.Error("qop=auth needs a cnonce")
				}
				if cred.Nc != 1 {
					t.Errorf("nc = %d, want 1 on a first answer", cred.Nc)
				}
			}
			if got, want := cred.Response, recompute(t, cred, "REGISTER", password); got != want {
				t.Errorf("response = %s, independently computed %s", got, want)
			}
			// The wrong password must not produce the same answer — the
			// whole point of the exchange.
			wrong, err := authorization(chal, "REGISTER", uri, user, "notit")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(wrong, cred.Response) {
				t.Error("a different password produced the same response")
			}
		})
	}
}

func TestAnUnusableChallengeIsRefusedRatherThanRetried(t *testing.T) {
	for _, tc := range []struct {
		name, header, value string
		code                int
	}{
		{name: "missing header", code: 401, header: "X-Nothing", value: "x"},
		{name: "unparseable", code: 401, header: "WWW-Authenticate", value: "Basic realm=x"},
		{
			// MD5-sess is legal SIP and we have no implementation of it.
			// Retrying sends the identical request, so this has to surface
			// as a provisioning fault, not a blip.
			name: "unsupported algorithm", code: 401, header: "WWW-Authenticate",
			value: `Digest realm="r", nonce="n", algorithm=MD5-sess, qop="auth"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := challengeResponse(t, tc.code, tc.header, tc.value)
			if _, _, err := challenge(res); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestResponseClassification(t *testing.T) {
	for _, tc := range []struct {
		code      int
		refused   bool
		whyItIsSo string
	}{
		{403, true, "the exchange knows us and says no"},
		{404, true, "no such line: provisioning, not weather"},
		{400, true, "our request is malformed; sending it again will not help"},
		{408, false, "a timeout is the network, and networks come back"},
		{480, false, "the exchange is not ready yet"},
		{503, false, "explicitly transient"},
		{500, false, "the far side broke, not us"},
	} {
		res := sip.NewResponse(tc.code, "because")
		err := responseError(res)
		var refused *Refused
		got := errorsAs(err, &refused)
		if got != tc.refused {
			t.Errorf("%d refused = %v, want %v (%s)", tc.code, got, tc.refused, tc.whyItIsSo)
		}
	}
}

// errorsAs keeps the table above readable.
func errorsAs(err error, target **Refused) bool {
	r, ok := err.(*Refused)
	if ok {
		*target = r
	}
	return ok
}

func TestGrantedExpiry(t *testing.T) {
	asked := time.Hour

	res := sip.NewResponse(200, "OK")
	if got := grantedExpiry(res, asked); got != asked {
		t.Errorf("with nothing said, the expiry is what we asked: %s", got)
	}

	// The exchange may shorten the binding, and the refresh must follow the
	// exchange rather than our request.
	res = sip.NewResponse(200, "OK")
	res.AppendHeader(sip.NewHeader("Expires", "120"))
	if got := grantedExpiry(res, asked); got != 120*time.Second {
		t.Errorf("Expires header ignored: %s", got)
	}

	// CUCM puts it on the Contact instead.
	res = sip.NewResponse(200, "OK")
	params := sip.NewParams()
	params.Add("expires", "300")
	res.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{Scheme: "sip", User: "201", Host: "10.0.0.5"},
		Params:  params,
	})
	if got := grantedExpiry(res, asked); got != 300*time.Second {
		t.Errorf("Contact expires parameter ignored: %s", got)
	}
}

func TestMinExpires(t *testing.T) {
	res := sip.NewResponse(423, "Interval Too Brief")
	if _, ok := minExpires(res); ok {
		t.Error("a 423 without Min-Expires has nothing to adopt")
	}
	res.AppendHeader(sip.NewHeader("Min-Expires", "3600"))
	if d, ok := minExpires(res); !ok || d != time.Hour {
		t.Errorf("minExpires = %s %v", d, ok)
	}
}
