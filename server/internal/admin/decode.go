package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Decode reads one JSON object from the request into v, strictly
// (ADMIN-API.md §4.2, §6 of the rules): the body is bounded by limit, must
// be application/json (or carry no Content-Type at all), valid UTF-8, one
// value with nothing after it, no duplicate keys at any depth, and no
// field v does not declare. On failure the §4.4 error has been written and
// Decode returns false; the handler simply returns. Nothing has been
// mutated, because nothing is mutated before Decode succeeds.
func Decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		mt, _, err := mime.ParseMediaType(ct)
		if err != nil || mt != "application/json" {
			WriteError(w, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "request body must be application/json")
			return false
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			WriteError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, fmt.Sprintf("request body over %d bytes", limit))
			return false
		}
		WriteError(w, http.StatusBadRequest, CodeBadJSON, "could not read the request body")
		return false
	}
	if code, msg, field := decodeStrict(body, v); code != "" {
		WriteFieldError(w, http.StatusBadRequest, code, msg, field)
		return false
	}
	return true
}

// decodeStrict is Decode's pure half: it returns the §4.4 code, message and
// field of the first fault, or "" when body decoded into v cleanly.
func decodeStrict(body []byte, v any) (code, msg, field string) {
	if len(bytes.TrimSpace(body)) == 0 {
		return CodeBadJSON, "empty request body; expected a JSON object", ""
	}
	if !utf8.Valid(body) {
		return CodeBadJSON, "request body is not valid UTF-8", ""
	}
	// Pass one: the token walk. It finds duplicate keys, which the standard
	// decoder silently resolves to the last value, and anything after the
	// first value.
	if code, msg, field := walk(body); code != "" {
		return code, msg, field
	}
	// Pass two: the typed decode, refusing unknown fields.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return classify(err)
	}
	return "", "", ""
}

// walk checks the JSON grammar once, tracking the keys seen in each open
// object so a duplicate is a fault and not a silent overwrite, and
// insists the body is exactly one value.
func walk(body []byte) (code, msg, field string) {
	dec := json.NewDecoder(bytes.NewReader(body))
	type frame struct {
		object bool
		keys   map[string]bool
		key    string // the key whose value is being read, for the path
		index  int    // array index, for the path
		expect bool   // in an object: the next token is a key
	}
	var stack []frame
	path := func() string {
		var b strings.Builder
		for _, f := range stack {
			if f.object {
				if b.Len() > 0 {
					b.WriteByte('.')
				}
				b.WriteString(f.key)
			} else {
				fmt.Fprintf(&b, "[%d]", f.index-1)
			}
		}
		return b.String()
	}
	first := true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			if first {
				return CodeBadJSON, "empty request body; expected a JSON object", ""
			}
			return "", "", ""
		}
		if err != nil {
			return classify(err)
		}
		if first {
			first = false
			if d, ok := tok.(json.Delim); !ok || d != '{' {
				return CodeBadJSON, "expected a JSON object", ""
			}
		} else if len(stack) == 0 {
			return CodeBadJSON, "unexpected data after the JSON object", ""
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				// A nested value: it is the current key's value in an
				// object, or the next element in an array.
				if n := len(stack); n > 0 && !stack[n-1].object {
					stack[n-1].index++
				}
				if d == '{' {
					stack = append(stack, frame{object: true, keys: map[string]bool{}, expect: true})
				} else {
					stack = append(stack, frame{})
				}
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expect = true
				}
			}
			continue
		}
		if len(stack) == 0 {
			continue
		}
		top := &stack[len(stack)-1]
		if top.object {
			if top.expect {
				k, _ := tok.(string)
				top.key = k
				if top.keys[k] {
					return CodeBadJSON, fmt.Sprintf("duplicate key %q", k), path()
				}
				top.keys[k] = true
				top.expect = false
				continue
			}
			// A scalar value for the current key.
			top.expect = true
		} else {
			top.index++
		}
	}
}

// classify maps an encoding/json error to the §4.4 code, message and field.
func classify(err error) (code, msg, field string) {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return CodeBadJSON, "malformed JSON: " + syn.Error(), ""
	case errors.As(err, &typ):
		return CodeInvalid, fmt.Sprintf("wrong type for %s: got %s", typ.Field, typ.Value), typ.Field
	case err == io.EOF, errors.Is(err, io.ErrUnexpectedEOF):
		return CodeBadJSON, "malformed JSON: unexpected end of input", ""
	}
	// encoding/json has no typed error for an unknown field; the message
	// is `json: unknown field "name"`.
	if s := err.Error(); strings.HasPrefix(s, "json: unknown field ") {
		name := strings.Trim(strings.TrimPrefix(s, "json: unknown field "), `"`)
		return CodeUnknownField, fmt.Sprintf("unknown field %q", name), name
	}
	return CodeBadJSON, "malformed JSON: " + err.Error(), ""
}

// ---- field validation helpers ----------------------------------------------

// Clean reports whether s has no control characters (NUL included) and is
// at most max bytes. Every free-text field an operator types goes
// through it before the store sees it.
func Clean(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return false
		}
	}
	return true
}

// Identifier grammars (§4.3), checked before any store or file path is
// touched. A path segment that fails is 404: it cannot name anything.
var (
	DeviceIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	ContactIDRe = regexp.MustCompile(`^ct_[0-9a-f]{1,64}$`)
	CallIDRe    = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,128}$`)
	DiagNameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,160}$`)
	UserRe      = regexp.MustCompile(`^[0-9A-Za-z._+-]{1,64}$`)
)

// ValidDiagName is DiagNameRe plus the two names that name a directory.
func ValidDiagName(s string) bool {
	return DiagNameRe.MatchString(s) && s != "." && s != ".."
}

// NotFound is the §4.4 404.
func NotFound(w http.ResponseWriter, what string) {
	WriteError(w, http.StatusNotFound, CodeNotFound, what+" not found")
}

// StoreError is the §4.4 500: the write failed and the entry is unchanged.
func StoreError(w http.ResponseWriter, err error) {
	WriteError(w, http.StatusInternalServerError, CodeStore, "store: "+err.Error())
}
