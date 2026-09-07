package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// fixturesDir is shared with the Swift package (protocol/fixtures).
const fixturesDir = "../../../protocol/fixtures"

// bodyFor returns a zero value of the typed body for each message type so the
// golden test can prove the typed structs round-trip every fixture field.
func bodyFor(t Type) any {
	switch t {
	case TypeHello:
		return &Hello{}
	case TypeWelcome:
		return &Welcome{}
	case TypeWake:
		return &Wake{}
	case TypeWakeAck:
		return &WakeAck{}
	case TypeWakeCancel:
		return &WakeCancel{}
	case TypeDirectoryChanged:
		return &DirectoryChanged{}
	case TypeError:
		return &Error{}
	case TypePing, TypePong:
		return &struct{}{}
	}
	return nil
}

func canonical(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonical unmarshal: %v", err)
	}
	return v
}

func TestGoldenFixturesRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixturesDir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures found in %s: %v", fixturesDir, err)
	}
	seen := map[Type]bool{}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}

			// 1. Frame it, read it back, validate.
			var buf bytes.Buffer
			var e Envelope
			if err := json.Unmarshal(raw, &e); err != nil {
				t.Fatalf("fixture is not an envelope: %v", err)
			}
			if err := WriteFrame(&buf, e); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !got.Type.Known() {
				t.Fatalf("fixture type %q is not a known v1 type", got.Type)
			}
			seen[got.Type] = true

			// 2. Whole envelope is semantically identical to the fixture.
			gotJSON, _ := json.Marshal(got)
			if !reflect.DeepEqual(canonical(t, raw), canonical(t, gotJSON)) {
				t.Fatalf("envelope drift\n fixture: %s\n got:     %s", raw, gotJSON)
			}

			// 3. Typed body decodes and re-encodes without losing fields.
			body := bodyFor(got.Type)
			if body == nil {
				t.Fatalf("no typed body registered for %q", got.Type)
			}
			if err := got.DecodeBody(body); err != nil {
				t.Fatalf("DecodeBody: %v", err)
			}
			re, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			want := canonical(t, []byte("{}"))
			if len(got.Body) > 0 {
				want = canonical(t, got.Body)
			}
			if !reflect.DeepEqual(want, canonical(t, re)) {
				t.Fatalf("typed body drift for %s\n fixture: %s\n typed:   %s", got.Type, got.Body, re)
			}
		})
	}
	for typ := range knownTypes {
		if !seen[typ] {
			t.Errorf("no golden fixture for message type %q", typ)
		}
	}
}

func TestNewProducesValidEnvelope(t *testing.T) {
	ts := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	e, err := New(TypeWakeAck, "id-1", ts, WakeAck{CallID: "c1", Action: WakeWillAnswer})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	var ack WakeAck
	if err := e.DecodeBody(&ack); err != nil {
		t.Fatal(err)
	}
	if ack.CallID != "c1" || ack.Action != WakeWillAnswer {
		t.Fatalf("body mismatch: %+v", ack)
	}
	p, err := New(TypePing, "id-2", ts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Body != nil {
		t.Fatalf("ping should have no body, got %s", p.Body)
	}
}

func TestValidateRejectsBadEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		e    Envelope
		want error
	}{
		{"wrong version", Envelope{V: 2, Type: TypePing, ID: "x"}, ErrUnsupportedVersion},
		{"no type", Envelope{V: 1, ID: "x"}, ErrMissingType},
		{"no id", Envelope{V: 1, Type: TypePing}, ErrMissingID},
	}
	for _, c := range cases {
		if err := c.e.Validate(); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}
	if err := (Envelope{V: 1, Type: "future_type", ID: "x"}).Validate(); err != nil {
		t.Errorf("unknown type must not fail Validate: %v", err)
	}
}

func TestFrameLimits(t *testing.T) {
	// Oversized length prefix is rejected before allocating.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrameSize+1)
	if _, err := ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	// Oversized envelope is refused on write.
	big := Envelope{V: 1, Type: TypeError, ID: "x", Body: json.RawMessage(`"` + string(bytes.Repeat([]byte("a"), MaxFrameSize)) + `"`)}
	if err := WriteFrame(io.Discard, big); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge on write, got %v", err)
	}
	// Clean EOF between frames is reported as io.EOF exactly.
	if _, err := ReadFrame(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("want io.EOF, got %v", err)
	}
	// Truncated payload is an error, not EOF.
	binary.BigEndian.PutUint32(hdr[:], 10)
	if _, err := ReadFrame(bytes.NewReader(append(hdr[:], 'a', 'b'))); err == nil || err == io.EOF {
		t.Fatalf("want truncated-payload error, got %v", err)
	}
}

func TestMultipleFramesOnOneStream(t *testing.T) {
	var buf bytes.Buffer
	ts := time.Now()
	for i := 0; i < 3; i++ {
		e, _ := New(TypePing, string(rune('a'+i)), ts, nil)
		if err := WriteFrame(&buf, e); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		e, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if e.ID != string(rune('a'+i)) {
			t.Fatalf("frame %d out of order: %s", i, e.ID)
		}
	}
	if _, err := ReadFrame(&buf); err != io.EOF {
		t.Fatalf("want io.EOF after last frame, got %v", err)
	}
}
