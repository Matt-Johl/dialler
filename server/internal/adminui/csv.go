package adminui

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strings"

	"dialler/server/internal/directory"
)

// CSV is the directory's file form (SPEC §4.8): display_name,uri,mode,
// favourite with a header row, UTF-8, RFC 4180 quoting. uri may be a bare
// number, which the server normalises; favourite is true/false and a
// missing column means false. The contact id is not in the file: the
// server reconciles by URI.

// WriteCSV renders contacts.
func WriteCSV(w io.Writer, contacts []directory.Contact) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"display_name", "uri", "mode", "favourite"}); err != nil {
		return err
	}
	for _, c := range contacts {
		fav := "false"
		if c.Favourite {
			fav = "true"
		}
		if err := cw.Write([]string{c.DisplayName, c.URI, string(c.Mode), fav}); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// MaxCSVBytes bounds an upload; the server's replace-all limit is 4 MiB.
const MaxCSVBytes = 4 << 20

// ReadCSV parses an upload. The header names the columns in any order;
// display_name, uri and mode are required, favourite optional. A UTF-8
// BOM is tolerated. Errors name the line.
func ReadCSV(r io.Reader) ([]directory.Contact, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxCSVBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxCSVBytes {
		return nil, fmt.Errorf("the file is over %d MiB", MaxCSVBytes>>20)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	cr := csv.NewReader(bytes.NewReader(raw))
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err == io.EOF {
		return nil, fmt.Errorf("the file is empty")
	}
	if err != nil {
		return nil, fmt.Errorf("line 1: %v", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, need := range []string{"display_name", "uri", "mode"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("the header has no %q column (want display_name,uri,mode,favourite)", need)
		}
	}
	get := func(rec []string, name string) string {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	var out []directory.Contact
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", line, err)
		}
		if len(rec) == 1 && rec[0] == "" {
			continue // a blank line
		}
		c := directory.Contact{DisplayName: get(rec, "display_name"), URI: get(rec, "uri"), Mode: directory.Mode(strings.ToLower(get(rec, "mode")))}
		if c.URI == "" {
			return nil, fmt.Errorf("line %d: uri is empty", line)
		}
		if c.Mode != directory.ModeLocal && c.Mode != directory.ModeTrunk {
			return nil, fmt.Errorf("line %d: mode must be local or trunk, not %q", line, get(rec, "mode"))
		}
		switch strings.ToLower(get(rec, "favourite")) {
		case "", "false", "0", "no":
		case "true", "1", "yes":
			c.Favourite = true
		default:
			return nil, fmt.Errorf("line %d: favourite must be true or false", line)
		}
		out = append(out, c)
	}
	if out == nil {
		out = []directory.Contact{}
	}
	return out, nil
}
