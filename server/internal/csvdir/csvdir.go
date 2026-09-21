// Package csvdir converts a device's directory to and from the CSV an
// administrator edits (SPEC §4.8): a header row, then one contact a row.
//
//	display_name,uri,mode,favourite
//
// UTF-8, RFC 4180 quoting (encoding/csv). uri may be a bare number, which
// becomes sip:<number>@<domain>; mode is local or trunk; favourite is
// true/false, and a file without that column means false throughout.
// Decoding refuses the whole file on the first bad row, with its line
// number, rather than applying half of it.
package csvdir

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"

	"dialler/server/internal/directory"
)

// Columns is the header, in order. Favourite is optional on input.
var Columns = []string{"display_name", "uri", "mode", "favourite"}

// Encode renders contacts as CSV with the header row.
func Encode(contacts []directory.Contact) []byte {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(Columns)
	for _, c := range contacts {
		fav := "false"
		if c.Favourite {
			fav = "true"
		}
		_ = w.Write([]string{c.DisplayName, c.URI, string(c.Mode), fav})
	}
	w.Flush()
	return buf.Bytes()
}

// Decode parses CSV into contacts ready for a replace-all. domain
// completes bare numbers.
func Decode(r io.Reader, domain string) ([]directory.Contact, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, errors.New("csvdir: empty file; the first row must be the header " + strings.Join(Columns, ","))
	}
	if err != nil {
		return nil, fmt.Errorf("csvdir: header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\xEF\xBB\xBF")))] = i
	}
	for _, required := range Columns[:3] {
		if _, ok := col[required]; !ok {
			return nil, fmt.Errorf("csvdir: header has no %q column (want %s)", required, strings.Join(Columns, ","))
		}
	}
	favCol, hasFav := col["favourite"]
	var out []directory.Contact
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("csvdir: line %d: %w", line, err)
		}
		if isBlank(rec) {
			continue
		}
		field := func(name string) string {
			i := col[name]
			if i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		c := directory.Contact{
			DisplayName: field("display_name"),
			URI:         NormalizeURI(field("uri"), domain),
			Mode:        directory.Mode(strings.ToLower(field("mode"))),
		}
		if c.URI == "" {
			return nil, fmt.Errorf("csvdir: line %d: uri is empty", line)
		}
		if c.Mode != directory.ModeLocal && c.Mode != directory.ModeTrunk {
			return nil, fmt.Errorf("csvdir: line %d: mode %q is not local or trunk", line, field("mode"))
		}
		if hasFav && favCol < len(rec) {
			switch strings.ToLower(strings.TrimSpace(rec[favCol])) {
			case "true", "yes", "1", "★":
				c.Favourite = true
			case "", "false", "no", "0":
			default:
				return nil, fmt.Errorf("csvdir: line %d: favourite %q is not true or false", line, rec[favCol])
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// NormalizeURI completes a bare number or user with the domain:
// "201" → "sip:201@dialler", "201@pbx" → "sip:201@pbx"; a full sip: or
// sips: URI is kept as it is.
func NormalizeURI(uri, domain string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}
	lower := strings.ToLower(uri)
	if strings.HasPrefix(lower, "sip:") || strings.HasPrefix(lower, "sips:") {
		return uri
	}
	if strings.Contains(uri, "@") {
		return "sip:" + uri
	}
	return "sip:" + uri + "@" + domain
}

func isBlank(rec []string) bool {
	for _, f := range rec {
		if strings.TrimSpace(f) != "" {
			return false
		}
	}
	return true
}
