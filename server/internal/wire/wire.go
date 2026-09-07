// Package wire implements the frozen v1 Dialler wire protocol: the JSON
// envelope, typed message bodies, and the length-prefixed TLS framing.
// See protocol/PROTOCOL.md. This package has no dependencies beyond the
// standard library and no knowledge of SIP, TLS, or the gateway.
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Version is the only envelope version this package speaks.
const Version = 1

// Type identifies a message.
type Type string

// Client → server.
const (
	TypeHello   Type = "hello"
	TypePing    Type = "ping"
	TypeWakeAck Type = "wake_ack"
)

// Server → client.
const (
	TypeWelcome          Type = "welcome"
	TypePong             Type = "pong"
	TypeWake             Type = "wake"
	TypeWakeCancel       Type = "wake_cancel"
	TypeDirectoryChanged Type = "directory_changed"
	TypeError            Type = "error"
)

var knownTypes = map[Type]bool{
	TypeHello: true, TypePing: true, TypeWakeAck: true,
	TypeWelcome: true, TypePong: true, TypeWake: true, TypeWakeCancel: true,
	TypeDirectoryChanged: true, TypeError: true,
}

// Known reports whether t is a message type defined by protocol v1.
// Unknown types are ignored by receivers, never fatal (PROTOCOL.md §2).
func (t Type) Known() bool { return knownTypes[t] }

// Envelope is the outer JSON object carried in every frame.
type Envelope struct {
	V    int             `json:"v"`
	Type Type            `json:"type"`
	ID   string          `json:"id"`
	TS   time.Time       `json:"ts"`
	Body json.RawMessage `json:"body,omitempty"`
}

// Errors returned by Validate.
var (
	ErrUnsupportedVersion = errors.New("wire: unsupported envelope version")
	ErrMissingID          = errors.New("wire: envelope id is empty")
	ErrMissingType        = errors.New("wire: envelope type is empty")
)

// Validate checks the envelope invariants that every receiver enforces.
// It does not reject unknown types.
func (e Envelope) Validate() error {
	if e.V != Version {
		return fmt.Errorf("%w: %d", ErrUnsupportedVersion, e.V)
	}
	if e.Type == "" {
		return ErrMissingType
	}
	if e.ID == "" {
		return ErrMissingID
	}
	return nil
}

// DecodeBody unmarshals the envelope body into v. A missing body decodes
// as an empty object so bodiless types (ping/pong) work with any struct.
func (e Envelope) DecodeBody(v any) error {
	if len(e.Body) == 0 {
		return json.Unmarshal([]byte("{}"), v)
	}
	return json.Unmarshal(e.Body, v)
}

// New builds an envelope of the given type with body marshalled from v.
// A nil body yields no "body" field.
func New(t Type, id string, ts time.Time, body any) (Envelope, error) {
	e := Envelope{V: Version, Type: t, ID: id, TS: ts.UTC()}
	if body == nil {
		return e, nil
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Envelope{}, fmt.Errorf("wire: marshal %s body: %w", t, err)
	}
	e.Body = raw
	return e, nil
}

// ---- Bodies ---------------------------------------------------------------

// ClientKind distinguishes the two connection kinds a device may hold.
type ClientKind string

const (
	ClientApp       ClientKind = "app"
	ClientExtension ClientKind = "extension"
)

// Hello is the first frame from a client.
type Hello struct {
	DeviceID     string     `json:"device_id"`
	Token        string     `json:"token"`
	Client       ClientKind `json:"client"`
	AppVersion   string     `json:"app_version,omitempty"`
	Capabilities []string   `json:"capabilities,omitempty"`
}

// SIPAccount tells a connected app how to register its SIP user agent so it
// is reachable directly while running (SPEC §2 foreground path). Additive
// field on Welcome; absent when the device is not bound to a SIP user.
type SIPAccount struct {
	User      string `json:"user"`
	Domain    string `json:"domain"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Transport string `json:"transport"` // always "tls" in v1
}

// Welcome acknowledges a successful Hello.
type Welcome struct {
	SessionID        string      `json:"session_id"`
	HeartbeatSeconds int         `json:"heartbeat_seconds"`
	ServerTime       time.Time   `json:"server_time"`
	DirectoryVersion int64       `json:"directory_version"`
	SIP              *SIPAccount `json:"sip,omitempty"`
}

// Party names one end of a call.
type Party struct {
	DisplayName string `json:"display_name,omitempty"`
	URI         string `json:"uri"`
}

// SIPTarget tells the app where to register for the call.
type SIPTarget struct {
	Host      string `json:"host"`
	Port      int    `json:"port"`
	Transport string `json:"transport"` // always "tls" in v1
}

// Wake announces an incoming call.
type Wake struct {
	CallID    string    `json:"call_id"`
	From      Party     `json:"from"`
	To        Party     `json:"to"`
	SIP       SIPTarget `json:"sip"`
	ExpiresAt time.Time `json:"expires_at"`
}

// WakeAction is the client's response to a Wake.
type WakeAction string

const (
	WakeWillAnswer WakeAction = "will_answer"
	WakeDecline    WakeAction = "decline"
	WakeBusy       WakeAction = "busy"
)

// WakeAck reports that the client surfaced (or refused) the call.
type WakeAck struct {
	CallID string     `json:"call_id"`
	Action WakeAction `json:"action"`
}

// CancelReason explains a WakeCancel.
type CancelReason string

const (
	CancelCallerHangup      CancelReason = "caller_hangup"
	CancelAnsweredElsewhere CancelReason = "answered_elsewhere"
	CancelTimeout           CancelReason = "timeout"
)

// WakeCancel tells the client to stop ringing.
type WakeCancel struct {
	CallID string       `json:"call_id"`
	Reason CancelReason `json:"reason"`
}

// DirectoryChanged tells the client the address book moved on.
type DirectoryChanged struct {
	Version int64 `json:"version"`
}

// ErrorCode enumerates protocol errors (PROTOCOL.md §8).
type ErrorCode string

const (
	CodeUnsupportedVersion ErrorCode = "unsupported_version"
	CodeBadFrame           ErrorCode = "bad_frame"
	CodeHelloExpected      ErrorCode = "hello_expected"
	CodeUnauthorized       ErrorCode = "unauthorized"
	CodeSuperseded         ErrorCode = "superseded"
	CodeIdleTimeout        ErrorCode = "idle_timeout"
	CodeUnknownCall        ErrorCode = "unknown_call"
)

// Error is sent by the server; if Fatal the connection closes after it.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message,omitempty"`
	Fatal   bool      `json:"fatal"`
}
