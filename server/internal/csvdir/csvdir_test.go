package csvdir

import (
	"bytes"
	"strings"
	"testing"

	"dialler/server/internal/directory"
)

func TestRoundTripWithQuotingAndUTF8(t *testing.T) {
	in := []directory.Contact{
		{DisplayName: "Zoë, Reception", URI: "sip:100@asterisk", Mode: directory.ModeTrunk, Favourite: true},
		{DisplayName: `He said "hi"`, URI: "sip:201@dialler", Mode: directory.ModeLocal},
		{DisplayName: "Plain", URI: "sip:202@dialler", Mode: directory.ModeLocal},
	}
	csv := Encode(in)
	if !strings.HasPrefix(string(csv), "display_name,uri,mode,favourite\n") {
		t.Fatalf("header: %q", csv)
	}
	if !strings.Contains(string(csv), `"Zoë, Reception",sip:100@asterisk,trunk,true`) || !strings.Contains(string(csv), `"He said ""hi""",sip:201@dialler,local,false`) {
		t.Fatalf("rows: %s", csv)
	}
	out, err := Decode(bytes.NewReader(csv), "dialler")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("%d rows", len(out))
	}
	for i := range in {
		if out[i].DisplayName != in[i].DisplayName || out[i].URI != in[i].URI || out[i].Mode != in[i].Mode || out[i].Favourite != in[i].Favourite {
			t.Fatalf("row %d: %+v vs %+v", i, out[i], in[i])
		}
	}
}

func TestDecodeAcceptsWhatAnAdministratorTypes(t *testing.T) {
	// A BOM from a spreadsheet, spaces, a bare number, a user@host, mixed
	// case, yes/no favourites, no favourite column at all, blank lines.
	src := "\xEF\xBB\xBFDisplay_Name, URI, Mode, Favourite\n" +
		"Desk phone, 100, Trunk, yes\n" +
		"Matt, 201@dialler, local, no\n" +
		"\n" +
		"Echo, sip:echo@dialler, LOCAL,\n"
	out, err := Decode(strings.NewReader(src), "asterisk")
	if err != nil {
		t.Fatal(err)
	}
	want := []directory.Contact{
		{DisplayName: "Desk phone", URI: "sip:100@asterisk", Mode: directory.ModeTrunk, Favourite: true},
		{DisplayName: "Matt", URI: "sip:201@dialler", Mode: directory.ModeLocal},
		{DisplayName: "Echo", URI: "sip:echo@dialler", Mode: directory.ModeLocal},
	}
	if len(out) != len(want) {
		t.Fatalf("%d rows: %+v", len(out), out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("row %d: %+v, want %+v", i, out[i], want[i])
		}
	}
	noFav, err := Decode(strings.NewReader("display_name,uri,mode\nA,1,local\n"), "d")
	if err != nil || len(noFav) != 1 || noFav[0].Favourite || noFav[0].URI != "sip:1@d" {
		t.Fatalf("without favourite column: %+v %v", noFav, err)
	}
}

func TestDecodeRefusesTheWholeFileOnABadRow(t *testing.T) {
	for _, tc := range []struct{ src, wantErr string }{
		{"", "empty file"},
		{"name,number\nA,1\n", `no "display_name" column`},
		{"display_name,uri,mode\nA,,local\n", "line 2: uri is empty"},
		{"display_name,uri,mode\nA,1,local\nB,2,carrier-pigeon\n", `line 3: mode "carrier-pigeon"`},
		{"display_name,uri,mode,favourite\nA,1,local,maybe\n", `line 2: favourite "maybe"`},
		{"display_name,uri,mode\n\"unterminated,1,local\n", "line 2"},
	} {
		_, err := Decode(strings.NewReader(tc.src), "d")
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%q: err %v, want %q", tc.src, err, tc.wantErr)
		}
	}
}

func TestNormalizeURI(t *testing.T) {
	for in, want := range map[string]string{
		"201": "sip:201@dialler", " 201 ": "sip:201@dialler", "201@pbx": "sip:201@pbx",
		"sip:201@x": "sip:201@x", "SIPS:201@x": "SIPS:201@x", "": "",
	} {
		if got := NormalizeURI(in, "dialler"); got != want {
			t.Errorf("NormalizeURI(%q) = %q, want %q", in, got, want)
		}
	}
}
