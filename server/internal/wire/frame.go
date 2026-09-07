package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxFrameSize is the largest payload a frame may carry (PROTOCOL.md §1).
const MaxFrameSize = 64 * 1024

// ErrFrameTooLarge is returned when a length prefix exceeds MaxFrameSize.
var ErrFrameTooLarge = errors.New("wire: frame exceeds MaxFrameSize")

// WriteFrame writes one envelope as a length-prefixed JSON frame.
func WriteFrame(w io.Writer, e Envelope) error {
	payload, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("wire: marshal envelope: %w", err)
	}
	if len(payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one length-prefixed frame and decodes the envelope.
// It does not call Validate; callers decide how to treat bad envelopes.
// io.EOF is returned unwrapped when the stream ends cleanly between frames.
func ReadFrame(r io.Reader) (Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.EOF {
			return Envelope{}, io.EOF
		}
		return Envelope{}, fmt.Errorf("wire: read frame header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return Envelope{}, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Envelope{}, fmt.Errorf("wire: read frame payload: %w", err)
	}
	var e Envelope
	if err := json.Unmarshal(payload, &e); err != nil {
		return Envelope{}, fmt.Errorf("wire: decode envelope: %w", err)
	}
	return e, nil
}
