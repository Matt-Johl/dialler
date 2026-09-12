# Dialler wire protocol — v1 (frozen)

This is the single protocol spoken between every client transport
(`WebSocketTransport` foreground LAN socket, `LPCTransport` extension socket,
later `APNSTransport`) and the light server's **signal gateway**. It carries
wake, session, and directory-change signalling only. **It never carries call
media or SIP.** Call setup and media travel on the separate SIP/TLS app leg
(SPEC §4.4).

Golden fixtures live in [`fixtures/`](fixtures/). Both the Go server and the
Swift shared framework must round-trip every fixture unchanged. Adding a message
type or field requires a new fixture; changing the meaning of an existing field
requires bumping `v`.

## 1. Transport and framing

- **Transport:** TLS 1.3 over TCP, client-initiated, server on port **7443** by
  default. Same framing on LAN and on the public edge. No UDP, no WebSocket.
- **Frame:** `uint32` big-endian payload length, followed by exactly that many
  bytes of UTF-8 JSON. One envelope per frame. Max payload **65536** bytes;
  a longer frame is a fatal protocol error and the connection is closed.
- **APNS delivery:** the push payload is the JSON envelope alone (no length
  prefix). Semantics are identical.

## 2. Envelope

```json
{
  "v": 1,
  "type": "wake",
  "id": "01J9ABCDEF0123456789ABCDEF",
  "ts": "2026-09-05T10:00:00Z",
  "body": { }
}
```

| Field | Type | Rule |
|---|---|---|
| `v` | int | Always `1`. A receiver MUST close with `error/unsupported_version` on any other value. |
| `type` | string | One of the message types below. Unknown types are ignored, not fatal, to allow forward-compatible additions. |
| `id` | string | Sender-unique message id (ULID recommended). Used for `wake_ack` correlation and de-duplication after reconnect. |
| `ts` | RFC 3339 UTC | Sender clock. Informational; receivers MUST NOT reject on skew. |
| `body` | object | Type-specific. Absent or `{}` for bodiless types. Unknown body fields are ignored. |

## 3. Message types

### Client → server

| Type | Body | Purpose |
|---|---|---|
| `hello` | `device_id`, `token`, `client` (`"app"` \| `"extension"`), `app_version`, `capabilities[]` | First frame on every connection. Authenticates the device. |
| `ping` | — | Liveness. Server answers `pong`. |
| `wake_ack` | `call_id`, `action` (`"will_answer"` \| `"decline"` \| `"busy"`) | Client has surfaced the call to CallKit (`will_answer`), or the user refused it / the device is busy — the server then answers the caller 486 Busy Here at once (§6 step 6). |

### Server → client

| Type | Body | Purpose |
|---|---|---|
| `welcome` | `session_id`, `heartbeat_seconds`, `server_time`, `directory_version`, `sip{user,domain,host,port,transport}` (optional) | Successful `hello`. Connection is now live. If `sip` is present the app SHOULD register that user agent immediately so calls reach it directly while it runs (SPEC §2 foreground path). |
| `pong` | — | Answer to `ping`. |
| `wake` | `call_id`, `from{display_name,uri}`, `to{display_name,uri}`, `sip{host,port,transport}`, `expires_at` | Incoming call. Client MUST report to CallKit immediately and then register SIP to `sip`. |
| `wake_cancel` | `call_id`, `reason` (`"caller_hangup"` \| `"answered_elsewhere"` \| `"timeout"`) | Stop ringing. |
| `directory_changed` | `version` | Address book changed on the server; client should sync. |
| `error` | `code`, `message`, `fatal` (bool) | Protocol or auth error. If `fatal`, the server closes after sending. |

Exactly one of `hello` (client) or `error` (server) is the first frame in each
direction. A server MUST NOT send `wake` before `welcome`.

## 4. Handshake

```
client                                   server
  │── TLS 1.3 ───────────────────────────►│
  │── hello{device_id, token, client} ───►│  validate credential
  │◄── welcome{session_id, heartbeat…} ───│  or error{fatal:true} + close
  │◄── directory_changed (if stale) ──────│
```

- `token` is the per-device credential issued at enrolment (SPEC §4.6). On the
  LAN today it is a long random opaque string; the format may change without
  a protocol bump because clients treat it as opaque.
- A device may hold **at most one connection per `client` kind**. A new
  `extension` connection replaces the old one (server closes the old with
  `error/superseded`). `app` and `extension` connections coexist: the server
  fans a `wake` out to **both** if both are connected; the client de-duplicates
  on `call_id`.

## 5. Heartbeat and liveness

- Server advertises `heartbeat_seconds` in `welcome` (default 25, chosen to sit
  under typical NAT UDP/TCP idle timeouts and iOS extension budgets).
- Client sends `ping` at that interval. Server replies `pong`.
- Server closes a connection with no frame received for `3 × heartbeat_seconds`.
- Either side may send `ping` at any time.

## 6. Wake flow

1. Server receives an inbound INVITE (from the PBX trunk or a local app) for a
   device. If the device holds a live SIP registration the INVITE is forwarded
   to it directly (the app rings from the INVITE); the server sends `wake` as
   well, so the app rings even if the INVITE is slow, and de-duplicates the
   two on `call_id` (the INVITE carries it in `X-Dialler-Call-ID`).
2. Otherwise the server sends `wake` on every connected transport for that
   device, and via APNS if none is connected (Phase 4b). `expires_at` is the
   ring deadline.
3. Client reports to CallKit **before** any network work, then replies
   `wake_ack{will_answer}`. The extension's job ends here; the main app is
   foregrounded by CallKit on answer.
4. The app registers SIP/TLS to `sip.host:sip.port`. The server bridges the
   legs (B2BUA). The `call_id` in the wake equals the `X-Dialler-Call-ID`
   header the server places on the app-leg INVITE, so the app can match them.
5. If the caller hangs up first, server sends `wake_cancel{caller_hangup}`.
6. If the client replies `wake_ack{decline}` or `wake_ack{busy}`, the server
   ends the caller leg immediately with **486 Busy Here**, so the PBX applies
   its busy rule (busy tone / forward-on-busy), and sends no `wake_cancel`
   (the device has already stopped ringing). Likewise an app that was
   reached by INVITE and rejects it with 486/600/603 has that status relayed
   to the caller unchanged. 480 is reserved for "nobody could be reached".
7. If no SIP registration arrives by `expires_at`, server sends
   `wake_cancel{timeout}` and releases the caller leg with 480 (busy /
   voicemail per trunk policy).

`wake` is **idempotent** per `call_id`: a client that reconnects may receive the
same wake again and MUST NOT ring twice.

## 7. Ordering and reconnect

- Frames on one connection are delivered in order. No ordering is guaranteed
  across the `app` and `extension` connections; `call_id` and `id` are the
  correlation keys.
- On reconnect the client sends a fresh `hello`. The server re-sends any
  `wake` whose `expires_at` is still in the future and whose call is still
  ringing: a wake is forgotten, silently, once its call has been answered
  and has ended (a `wake_cancel` would end the live call in a client that
  answered it). There is no other replay; the directory is re-synced via
  `directory_version`.
- A connection drop is not a call event for the client. A ringing `wake`
  does not depend on the connection that carried it (the extension's
  connection may have delivered it via PushKit while the app's own socket
  was dead), and the INVITE arrives through the SIP registration the client
  makes on answer, not through this channel. A client MUST NOT end or
  disarm a ringing or answered call because its own connection dropped.
- There is no session resume in v1. `session_id` is informational (logging,
  support). Resume for cellular hand-off is a v1.1 additive extension
  (Phase 4b) and will be signalled by a `resume_token` field in `welcome`.

## 8. Error codes

| Code | Fatal | Meaning |
|---|---|---|
| `unsupported_version` | yes | `v` ≠ 1 |
| `bad_frame` | yes | Frame too large or not JSON |
| `hello_expected` | yes | First frame was not `hello` |
| `unauthorized` | yes | Unknown device or bad token |
| `superseded` | yes | Replaced by a newer connection of the same kind |
| `idle_timeout` | yes | No frame within 3 × heartbeat |
| `unknown_call` | no | `wake_ack` for a `call_id` the server no longer has |

## 9. Versioning rules

- Additive fields and new message types: **no bump**. Receivers ignore unknowns.
- Renaming, removing, or changing the meaning of a field: **bump `v`**. A
  server may support several `v` values concurrently; the client's `v` in
  `hello` selects the dialect for that connection.
