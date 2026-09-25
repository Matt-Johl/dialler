# Dialler admin contract — v1 (SPEC §6 item 9a)

*Written 2026-09-25 on `feature/admin-api`; awaiting approval.* This is the
deliverable of item 9a: the configurables an operator needs to run and
maintain a Dialler site, and the call server's admin API that follows from
them. Item 9b builds and hardens exactly this API; item 9c builds
`dialler-admin` against it, and nothing else. The UI is deliberately not
designed here — someone may run a CLI or their own provisioning against
the same API — beyond §9, which records what 9c must do with the answers
the API gives it.

The reading order is: §1 the rules everything below obeys; §2 the
inventory of everything that can be configured, with where it lives and
how it changes; §3 the inventory of everything that can be observed; §4
the API conventions; §5 the API, resource by resource; §6 wire and
persistence changes; §7 what 9b changes in the harness; §8 the decisions
this document settles; §10 what is recorded and not done.

Throughout, **today** means the server on `main` at cd8fc28, and **9b**
marks what does not exist yet.

## 1. Rules

1. **One process holds the truth.** `dialler-server` owns every device
   record, directory, credential and setting. `dialler-admin` holds nothing
   but its own operator password and session table; it can be destroyed
   and re-deployed without loss. Nothing in this contract puts state in
   the admin process that the API cannot re-read.
2. **An admin action never interrupts a call and affects only the device
   it names** (SPEC §4.8). Every write is an ordinary handler call that
   takes a store lock, changes one device's entry, rewrites that device's
   file atomically and notifies that device's live sessions. Nothing here
   restarts, reloads, re-binds a listener or re-reads a global file. The
   two deliberate exceptions are §5.8 (ending one named call) and §5.10
   (the log level), and each says what it touches.
3. **Startup configuration is the deployment's, not the API's.** The
   flags in §2.1 define what this server *is*: its addresses, certificate,
   domain, PBX. They are set in the unit file, read once, and shown by the
   API read-only with their effective values. The API persists no
   server-wide setting, so an operator can always reason about a server
   from its unit file plus the data directory, and rule 2 holds by
   construction. What an operator changes day to day is per device, and
   that is what the API writes.
4. **The API authenticates a caller, not a person.** Bearer token over TLS
   on a listener of its own (§4.1); no operator identity crosses it and
   the call server keeps no audit trail (SPEC 9a, settled 2026-09-23). The
   event ring of §5.9 records what the *server* did, never who asked.
5. **Additive within v1.** Every route below that exists today keeps its
   path, verbs, success status and success body. New fields are added,
   never renamed. Error *bodies* change from plain text to the JSON
   envelope of §4.4, which is the one shape change, and it is allowed
   because no caller in the tree parses an error body (`provision.sh`
   prints it). A change that cannot be made additively gets `/v2/`.
6. **Strict at the boundary.** One typed request struct per endpoint,
   `DisallowUnknownFields`, a bounded body, validation before any
   mutation, and a JSON error naming the field. A malformed request is a
   400 and nothing else happens. This is what 9b's fuzzing proves.
7. **Secrets go in and never come out.** The admin token, a device's
   token, a PBX line secret, the sealing key and the trunk's private key
   are never in any response, event, or log line. A device token is
   returned exactly once, on issue, and only when the caller supplied it
   (the harness fixtures).

## 2. Configurables

Scope says what a value belongs to. *Where* says where the truth lives.
*Changed by* says how an operator changes it and when the change takes
effect. "Restart" means edit the unit file and restart the service, which
drops every call and session; §10 says why that is acceptable for the
things it applies to.

### 2.1 Deployment (startup flags, read-only through the API)

Shown by `GET /v1/admin/server` (§5.7) with their effective values, after
defaults have been resolved (`-public-host` shows the address that was
picked, not the empty string). None is writable through the API.

| Flag | Default | What it is | Why it is a restart |
|---|---|---|---|
| `-signal-addr` | `:7443` | Wire-protocol TLS listener | A listener |
| `-sip-addr` | `:5061` | App-leg SIP/TLS listener | A listener |
| `-http-addr` | `127.0.0.1:8080` | Device API listener (`/v1/directory`, `/v1/diag`, `/v1/enrol`, `/healthz`) | A listener |
| `-admin-addr` *(9b)* | `127.0.0.1:8081` | Admin API listener (`/v1/admin/`, `/healthz`) | A listener |
| `-admin-token-file` *(9b)* | — | File holding the admin bearer token; `-admin-token` remains for the harness and is deprecated for production | Read once; rotating it is a restart of both processes |
| `-tls-cert` / `-tls-key` | self-signed under `<data>/tls` | The certificate every phone pins | Rotation strands every enrolled phone (§10.1) |
| `-public-host` | first non-loopback IPv4 | Host advertised in `welcome`, wakes, the QR link and the self-signed SANs | Baked into the certificate and every welcome |
| `-local-domain` | `= public-host` | SIP domain and Digest realm | HA1s are computed with it; a change invalidates every credential |
| `-data-dir` | `./data` | Persistence root (§6.2) | Everything |
| `-ring-timeout` | 30s | How long a callee rings, and how long a woken app may take to register | Read into the B2BUA config at start |
| `-peer-timeout` | 60s | In-dialog OPTIONS liveness per leg; 0 off | Same |
| `-trunk-qualify` | 10s | OPTIONS probe interval on a TCP/TLS trunk; 0 off | A goroutine started at Serve |
| `-rtp-min` / `-rtp-max` | 20000 / 20100 | Relay UDP port range | Process globals in the media library; a firewall rule |
| `-rtp-symmetric`, `-rewrite-contact` | true | NAT behaviour | Harness negatives only |
| `-public-sip-port`, `-public-http-port` | 0 | Advertised ports behind a container | Advertised in welcome and QR |
| `-trunk` | "" (standalone) | The PBX peer URI | Defines the PBX leg |
| `-trunk-addr`, `-trunk-external-host` | | Trunk listener and advertised address | A listener |
| `-trunk-srtp` | off | `off` or `sdes` on the PBX leg | Per-call at offer time, but a site-wide security posture (§10.3) |
| `-trunk-codecs` | `g722,pcmu,pcma` | Offer order to the PBX | Same |
| `-trunk-tls-cert/-key/-ca`, `-trunk-tls-insecure`, `-trunk-tls-min-version` | | PBX-leg TLS | Loaded into a `tls.Config` at start |
| `-pbx-mode` | trunk | `trunk` or `lines` | Defines which code path exists |
| `-pbx-registrar`, `-pbx-domain`, `-pbx-peers`, `-pbx-register-expiry`, `-pbx-default-line` | | Lines-mode parameters | Read into the line manager at start |
| `-log-json` | false | Log format | Handler chosen at start |
| `-log-level`, `-sip-trace` | info, false | Initial verbosity | The *initial* value only; §2.3 overrides it live |
| `-diag-retain` *(9b)* | `30d` | How long a device's diagnostic uploads are kept; `0` keeps forever | A sweeper started at Serve |
| `-version` *(9b)* | | Print the build version and exit | — |

*What is deliberately not a flag:* the enrolment code lifetime (fifteen
minutes, SPEC §4.8), the code alphabet, the heartbeat interval, the
frame limit, the enrol rate limit. These are protocol constants that the
app also assumes.

### 2.2 Per device (the API's writable surface)

All persisted in the device's entry in `devices.json` and its own
directory file, and all writable through §5.

| Configurable | Where | Changed by | Effect on the device |
|---|---|---|---|
| **Existence**: device id, user (extension) | `devices.json` entry | `POST /v1/admin/devices`; `DELETE …?purge=1` | Registry provisioned or removed. **User is immutable** (§8.3) |
| **Label** | entry `label` | `PATCH /v1/admin/devices/{id}` | None; the app never sees it |
| **Credential** | entry `token_hash`, `ha1` | A claim (`POST /v1/enrol`), or `POST /v1/admin/devices` with a fixture `token` | Sessions on the old credential are dropped with `error/unauthorized` |
| **Revoked** | entry `revoked` | `DELETE /v1/admin/devices/{id}` sets it; a claim clears it | Sessions dropped, registration removed, PBX line unregistered; a call it is on runs to its end |
| **Enrolment code** | entry `code_hash`, `code_expires` | `POST …/enrol-code` mints; `DELETE …/enrol-code` cancels | None until claimed |
| **Server-managed settings**: SSIDs | entry `ssids`, `config_version` | `PUT …/config` | `config` pushed to the device's sessions; the welcome carries it |
| **PBX line**: DN, digest user, secret | entry `pbx_line` (secret sealed) | `PUT` / `DELETE …/pbx-line` | That one line re-registers or unregisters; nothing pushed to the device |
| **Directory**: contacts, favourites | `directories/<id>.json` | `PUT …/directory` (replace-all), `POST`, `PUT` / `DELETE …/directory/{cid}` | `directory_changed` pushed with the new version |
| **Diagnostic uploads** | `diag/<id>/…` | Written by the device; `DELETE …/diag[/{name}]` | None |

### 2.3 Server-wide, runtime, not persisted

The one class of server-wide value the API writes: diagnostics
verbosity. It is a maintenance switch — "turn on debug while I reproduce
this" — and is *not persisted*, so a restart returns to the flags and a
forgotten switch cannot outlive the operator's session. A write may carry
a duration after which the server reverts on its own.

| Configurable | Changed by | Effect |
|---|---|---|
| Log level (`debug`, `info`, `warn`, `error`) | `PUT /v1/admin/log` | Every subsequent log line, in every package, via one `slog.LevelVar` |
| SIP trace (bool) | `PUT /v1/admin/log` | The global `sip.SIPDebug` |

### 2.4 Not configurable, and why

- **Hold music, ring-back and busy tones** are embedded in the binary
  (`internal/moh`). A site that wants its own music is a build; recorded
  in §10 as an option, not work.
- **Call history**: the server keeps no call records (SPEC §3). The
  active-call list of §5.8 is live state, gone when the call ends.
- **The enrolment rate limit** (five attempts a minute per source) and
  the **admin rate limit** (§4.6) are constants.

## 3. Observables

What an operator can see, and where the server holds it today. Items
marked *9b* have no accessor yet and must be added; the rest is a read of
existing memory.

| Observable | Source today | Exposed by |
|---|---|---|
| Device record: label, user, issued, revoked, enrolled, code pending and its expiry, settings, line (no secret) | `enroll.Store.Devices()` | `GET /v1/admin/devices`, `GET …/{id}` |
| App and extension sessions: online, since when, remote address, `app_version` from `hello` | `gateway.sessions` — *9b: record since, address and app_version; add a snapshot method* | `GET /v1/admin/status` |
| SIP registration: registered, contact, expires | `registry.LookupDevice` | `GET /v1/admin/status` |
| PBX line state: pending / registered / retrying / refused, since, expiry, realm, last error, attempts | `pbxline.Manager.Statuses()` — written, tested, **no caller** | `GET /v1/admin/status` |
| Trunk qualify: up / down / off, since, last error | a local `dead bool` in `trunkQualifier.run` — *9b: expose it* | `GET /v1/admin/status`, `GET /v1/admin/server` |
| Active calls: parties, legs, codec, SRTP, state, since, relay counters | `bridgedCall` and `pump` counters exist per goroutine; **no registry of live calls** — *9b: add one* | `GET /v1/admin/calls` |
| Server identity: version, build, started, uptime, mode, effective flags, listeners, certificate fingerprint and `not_after`, data dir | scattered; *9b: `-X main.version` in the Makefile* | `GET /v1/admin/server` |
| Fleet counts: devices, enrolled, revoked, online, registered, lines registered, calls | derived | `GET /v1/admin/server` |
| Recent events: presence, registration, line and trunk transitions, calls, enrolment, admin writes | log lines only — *9b: a bounded ring* | `GET /v1/admin/events` |
| Diagnostic uploads per device: name, kind, size, time | files under `diag/<id>/` | `GET …/diag`, `GET …/diag/{name}` |
| Liveness | `GET /healthz` | Both listeners |
| Current log level and trace | `slog.LevelVar`, `sip.SIPDebug` | `GET /v1/admin/log` |

## 4. API conventions

### 4.1 Listener and authentication (from 9b, restated as contract)

- The admin API is served on **`-admin-addr`**, default `127.0.0.1:8081`,
  on the same certificate as the other listeners. `/v1/admin/` is **not**
  served on `-http-addr`: a request for it there is 404. Production runs
  both processes on one host, so loopback is the default and the admin API
  is unreachable from the office network. Testing may bind it wider and
  the token over TLS protects it, as it protects the device API.
- Every `/v1/admin/` request carries `Authorization: Bearer <token>`,
  compared in constant time against the token from `-admin-token-file`
  (or `-admin-token`). A missing or wrong token is `401
  {"error":"unauthorized"}` with no other information, before any path or
  body handling. The token is never logged.
- `GET /healthz` on the admin listener is unauthenticated and answers
  `200 ok`.
- `GET /v1/admin/whoami` *(9b)* is the cheapest authenticated call:
  `200 {"ok":true}`. It is how `dialler-admin` checks its token at start
  and how the UI tells "wrong token" from "server down".

### 4.2 Content

- Request bodies are `application/json`, UTF-8. A body with any other
  `Content-Type` is 415. Success bodies are JSON; `204` has none.
- Decoding is strict: unknown fields, duplicate keys, wrong types and
  trailing data are 400 with the field named (§4.4). Every body has a
  limit (per route, §5); over it is 413.
- Times are RFC 3339 UTC. Durations are seconds as integers.
- JSON success bodies are written by `encoding/json` with map keys
  sorted, as today; `harness/wake_test.sh` depends on `device_id` sorting
  before `token`, so responses that carry both stay maps or keep that
  order.

### 4.3 Identifiers

| Id | Grammar | Where it appears |
|---|---|---|
| Device id | `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` — generated ids are `dev_` + six Crockford characters; the harness uses `dev-a` | `{id}` in paths, `device_id` in bodies |
| Contact id | `^ct_[0-9a-f]{1,64}$` | `{cid}` in paths |
| Call id | opaque, `^[A-Za-z0-9._@-]{1,128}$` | `{call}` in paths |
| Diag file name | `^[A-Za-z0-9._-]{1,160}$`, never `.` or `..` | `{name}` in paths |

A path segment that fails its grammar is 404, not 400: it cannot name
anything, and the check runs before the store is asked so a traversal
attempt never reaches a file path.

### 4.4 Errors

Every non-2xx answer on `/v1/admin/` carries:

```json
{"error": "device_exists", "message": "dev-a already exists", "field": "device_id"}
```

| Field | Rule |
|---|---|
| `error` | A stable snake_case code from the table below. Callers switch on this, never on `message`. |
| `message` | One sentence for a human. May change. |
| `field` | Present on 400 when one JSON field is at fault: its name, dotted for nesting (`contacts[3].uri`). |

| Status | `error` | When |
|---|---|---|
| 400 | `bad_json` | Not JSON, trailing data, duplicate key |
| 400 | `unknown_field` | A field the endpoint does not take |
| 400 | `invalid` | A field's value is wrong (`field` says which) |
| 400 | `missing` | A required field is absent |
| 401 | `unauthorized` | Bad or missing bearer |
| 404 | `not_found` | Unknown device, contact, call, file; or a path segment that fails its grammar |
| 405 | `method_not_allowed` | With `Allow` |
| 409 | `device_exists` | `POST /v1/admin/devices` on an id that exists, without a `token` (§5.1) |
| 409 | `user_taken` | Another device already has that user |
| 409 | `immutable` | An attempt to change `user` |
| 412 | `version_mismatch` | `If-Match` did not match (§4.5) |
| 413 | `too_large` | Body over the route's limit |
| 415 | `unsupported_media_type` | |
| 429 | `rate_limited` | With `Retry-After` |
| 500 | `store` | A write to the data directory failed; the entry is unchanged (atomic rename) |
| 503 | `unavailable` | The server cannot do this now: no sealing key (`pbx.key` unreadable), or lines mode is off for a line route that needs it |

### 4.5 Concurrency: versions and `If-Match`

Every resource that carries a `version` accepts an optional
`If-Match: "<version>"` on a write. A mismatch is 412 and nothing
changes. Without the header a write is last-write-wins, as today. This is
what lets a UI refuse to overwrite an edit made in another tab without
forcing the harness to track versions.

| Resource | Version |
|---|---|
| A device's directory (replace-all and per-contact writes) | the directory version from `GET …/directory` |
| A device's settings | `config.version` |
| A device's record (label, line) | `updated_at` of the record, as an RFC 3339 string *(9b: add `updated_at` to the record)* |

### 4.6 Rate limit

The admin listener takes at most **50 requests a second per source
address, burst 100**; over it is 429 with `Retry-After: 1`. This is far
above any UI and any provisioning script, and below what would let a
misbehaving caller busy the store lock. The device listener's `/v1/enrol`
limit is unchanged.

### 4.7 Scale

**Lists return everything. No pagination in v1.** The design ceiling is
**500 devices**: a status document for 500 devices is under 200 KB, a
directory of 2,000 contacts under 500 KB, both trivial for a LAN and a
browser. Filtering and sorting are the client's. The query parameters
`limit` and `cursor` are **reserved** on every list route and refused
with 400 `invalid` today, so a v1 client that never sends them keeps
working if a later version honours them.

## 5. The API

Routes are grouped by resource. Each says: method and path; body and its
limit; success; errors beyond §4.4's generic ones; *effects*, meaning what
the write touches and what the device sees; and *today*, meaning whether
it exists on `main`.

### 5.1 Devices

**`GET /v1/admin/devices`** → `200 [Device]`. *Today: exists.* Includes
revoked devices. `Device` gains `updated_at` and `code_expires_at` (9b,
additive):

```json
{
  "device_id": "dev_7K3M9Q", "user": "204", "label": "Warehouse 3",
  "issued_at": "…", "updated_at": "…",
  "revoked": false, "enrolled": true,
  "code_pending": true, "code_expires_at": "…",
  "config": {"version": 3, "ssids": ["Office"]},
  "pbx_line": {"dn": "", "digest_user": "line204", "configured": true, "updated_at": "…"}
}
```

**`GET /v1/admin/devices/{id}`** → `200 Device`. *9b.* The same object;
404 for unknown.

**`POST /v1/admin/devices`** — body ≤ 4 KiB
`{"user": "204", "device_id"?: "…", "label"?: "…", "token"?: "…"}` →
`201 {"device_id","user","label","code","expires_at","url"[,"token"]}`.
*Today: exists, with two defects this contract removes.*

- `user` required, `^[0-9A-Za-z._+-]{1,64}$`; 409 `user_taken` if another
  device has it.
- `device_id` absent → generated. Present and **new** → created as given.
- Present and **existing, no `token`** → **409 `device_exists`**. Today
  this silently overwrites the label with `""`; `provision.sh` line 73
  relies on it being accepted, and 9b changes that line to
  `POST …/dev-a/enrol-code` (§7).
- Present and **existing, with `token`** → the credential is replaced and
  **everything else is kept**: label, settings, line, pending code,
  directory. Today `IssueToken` builds a fresh record and keeps only the
  label, so re-provisioning the harness wipes a line and its SSIDs. This
  is what the fixtures need: same id, same user, fixed token, nothing
  else disturbed. `user` must equal the stored one or 409 `immutable`.
- `token`, when given, is ≥ 16 characters and is returned once.
- Always mints a code (15 min) and returns `code`, `expires_at`, `url`.

*Effects:* the registry is provisioned for `user`; if a credential was
replaced, sessions on the old one are dropped with `error/unauthorized`.
Nothing else on the server is touched.

**`PATCH /v1/admin/devices/{id}`** — body ≤ 4 KiB `{"label": "…"}` →
`200 Device`. *9b.* `label` ≤ 120 characters, may be empty. `user` in the
body is 409 `immutable` (§8.3). Honours `If-Match` on `updated_at`.
*Effects:* the entry, nothing else.

**`DELETE /v1/admin/devices/{id}`** → `204`. *Today: exists.* Revoke:
the record stays with `revoked: true`, directory, settings and line
config are kept. *Effects:* registry deprovisioned, sessions dropped with
`error/unauthorized`, the PBX line unregistered; a call the device is on
runs to its end. Idempotent: revoking a revoked device is 204.

**`DELETE /v1/admin/devices/{id}?purge=1`** → `204`. *9b.* Everything
above, then the record, `directories/<id>.json` and `diag/<id>/` are
deleted. The user is free for a new device at once. 404 for unknown.

**Revoked devices and every other route.** *Decided here:* `revoked`
affects only authentication, registration and the line. Every admin
route treats a revoked device as present: reads succeed, writes succeed,
so an operator can prepare a replacement phone's settings, line and
directory before minting its code, and re-enrolment restores all of it.
Today `GET …/config`, the line routes and the directory routes answer
404 for a revoked device while `PUT …/config` succeeds; 9b makes them
all succeed. 404 is for unknown or purged only.

### 5.2 Enrolment codes

**`POST /v1/admin/devices/{id}/enrol-code`** →
`200 {"code","expires_at","url"}`. *Today: exists.* Replaces any pending
code. Works on a revoked device: the claim un-revokes. *Effects:* the
entry.

**`DELETE /v1/admin/devices/{id}/enrol-code`** → `204`. *9b.* Cancels a
pending code; 204 also when none is pending. *Effects:* the entry.

### 5.3 Device settings

**`GET /v1/admin/devices/{id}/config`** → `200 {"version","ssids"}`;
404 `not_found` until set. *Today: exists.*

**`PUT /v1/admin/devices/{id}/config`** (POST accepted, for busybox) —
body ≤ 16 KiB `{"ssids": ["Office", "Office-5G"]}` → `200 {"version","ssids"}`.
*Today: exists.* `ssids` required (may be empty: that removes Local Push
on the phone, SPEC 8b); each ≤ 32 bytes, trimmed, de-duplicated, at most
32 of them. Honours `If-Match` on `version`. **Identical list → no
version bump, no push, 200 with the current version** (today it bumps;
9b aligns it with the directory's rule so a re-applied form disturbs
nobody). *Effects:* the entry; `config{version, ssids}` pushed to that
device's live sessions.

### 5.4 PBX line

**`GET /v1/admin/devices/{id}/pbx-line`** →
`200 {"dn","digest_user","configured":true,"updated_at"}`; 404 until
set. *Today: exists.* Never the secret. In `trunk` mode the routes work
(a line may be provisioned ahead of a switch to lines mode) and the
status simply reports no registration.

**`PUT /v1/admin/devices/{id}/pbx-line`** (POST accepted) — body ≤ 4 KiB
`{"digest_user": "…", "secret": "…", "dn"?: "…"}` → `200` the view above.
*Today: exists.* `digest_user` and `secret` required, each ≤ 128 bytes,
no control characters; `dn` ≤ 32, digits and `+*#` only. Honours
`If-Match` on `updated_at`. 503 `unavailable` if the sealing key cannot
be read. *Effects:* the entry (secret sealed with `pbx.key`); in lines
mode, **that one line** unregisters and re-registers with the new
credential and no other line is touched; nothing is pushed to the
device.

**`DELETE /v1/admin/devices/{id}/pbx-line`** → `204`, also when none.
*Today: exists.* *Effects:* the entry; that one line unregisters.

### 5.5 Directory

**`GET /v1/admin/devices/{id}/directory`** →
`200 {"version": 41, "contacts": [Contact]}`, live contacts only.
*Today: exists.* `Contact` is
`{"id","display_name","uri","mode":"local"|"trunk","favourite","version","updated_at"}`.

**`PUT /v1/admin/devices/{id}/directory`** — body ≤ 4 MiB
`{"contacts": [{"display_name","uri","mode","favourite"?}]}` →
`200 {"version","added","changed","removed"}`. *Today: exists.*
Replace-all: reconciled by URI; upsert what changed, tombstone what is
missing, one version bump; **an identical list is a no-op** (200, same
version, zeros, no push). At most **5,000 contacts**; over it is 400
`invalid` on `contacts`. Each `uri` ≤ 256 bytes, either `sip:user@host`
or a bare number the server normalises to `sip:<n>@<local-domain>`;
`display_name` ≤ 120; `mode` one of the two; a duplicate URI in the
request is 400 `invalid` naming the index. Honours `If-Match` on the
directory version. *Effects:* the device's file; `directory_changed`
pushed to that device only, and only if something changed.

**`PUT /v1/admin/devices/{id}/directory?dry_run=1`** → the same counts
and the *current* version, **nothing written, nothing pushed**. *9b.*
This is what shows an operator "12 added, 3 changed, 40 removed" before
an upload applies, without the client having to reimplement the
reconcile.

**`POST /v1/admin/devices/{id}/directory`** — body ≤ 64 KiB, one
`Contact` without `id` → `200 Contact`. *Today: exists.* Upsert by URI
(the harness seeds this way). **`PUT …/directory/{cid}`** → `200
Contact`, 404 for an unknown or deleted `cid`. **`DELETE
…/directory/{cid}`** → `204`, 404 for unknown. All honour `If-Match` on
the directory version. *Effects:* as the replace-all, one contact.

*"Copy directory to…"* is not a server route. It is `GET` on the source
and a `PUT` replace-all on each target, one call per target, and §9
says how the client reports a partial failure. A server-side fan-out
would be the one admin action that touches N devices' files, which rule
2 forbids.

### 5.6 Fleet status

**`GET /v1/admin/status`** *(9b; SPEC §4.8, the route 3c stopped short
of)* → `200`:

```json
{
  "at": "2026-09-25T09:00:00Z",
  "trunk": {"configured": true, "qualify": "up", "since": "…", "last_error": ""},
  "devices": [
    {
      "device_id": "dev_7K3M9Q", "user": "204", "label": "Warehouse 3",
      "revoked": false, "enrolled": true,
      "sessions": {
        "app":       {"online": true, "since": "…", "addr": "10.18.0.41:52011", "app_version": "1.4 (212)"},
        "extension": {"online": true, "since": "…", "addr": "10.18.0.41:52012", "app_version": "1.4 (212)"}
      },
      "sip": {"registered": true, "contact": "sip:204@10.18.0.41:52013;transport=tls", "expires_at": "…"},
      "line": {"state": "registered", "since": "…", "expires_at": "…", "realm": "asterisk", "error": "", "attempts": 0},
      "call": {"call_id": "…", "since": "…"}
    }
  ]
}
```

- `sessions.app` / `sessions.extension` are absent when offline.
- `sip` is absent when not registered.
- `line` is absent when the device has no line, or the server is not in
  lines mode; its shape is `pbxline.Status` minus `user`, `dn` and
  `digest_user`, which the record already carries.
- `call` is absent when the device is not on a call; `GET /v1/admin/calls`
  has the rest.
- `trunk.qualify` is `"up"`, `"down"`, `"off"` (qualify disabled or a
  UDP trunk) or absent when `trunk.configured` is false.

This is one read of what `gateway`, `registry`, `pbxline` and the B2BUA
hold in memory. It takes no store lock and never blocks a call.

### 5.7 Server

**`GET /v1/admin/server`** *(9b)* → `200`:

```json
{
  "version": "0.9.3+g1a2b3c4", "go": "go1.25", "started_at": "…", "uptime_seconds": 86400,
  "mode": "lines",
  "listeners": {"signal": ":7443", "sip": ":5061", "http": "10.18.0.10:8080", "admin": "127.0.0.1:8081", "trunk": ":5062"},
  "public_host": "10.18.0.10", "local_domain": "dialler", "public_sip_port": 5061, "public_http_port": 8080,
  "tls": {"self_signed": false, "subject": "CN=dialler.example", "not_before": "…", "not_after": "…", "fingerprint_sha256": "…"},
  "trunk": {
    "uri": "sip:asterisk:5062;transport=tls", "transport": "tls", "srtp": "sdes", "codecs": ["g722","pcmu","pcma"],
    "qualify_interval_seconds": 10, "qualify": "up", "qualify_since": "…", "qualify_last_error": "",
    "tls": {"cert_set": true, "ca_set": true, "insecure": false, "min_version": "1.2"}
  },
  "pbx": {"registrar": "asterisk:5062", "domain": "asterisk", "peers": [], "register_expiry_seconds": 3600, "default_line": ""},
  "ring_timeout_seconds": 30, "peer_timeout_seconds": 60,
  "rtp": {"min": 20000, "max": 20100, "symmetric": true},
  "data_dir": "/var/lib/dialler", "diag_retain_seconds": 2592000,
  "counts": {"devices": 42, "enrolled": 40, "revoked": 2, "app_online": 31, "extension_online": 38, "sip_registered": 30, "lines_registered": 38, "lines_failed": 1, "calls": 3}
}
```

Read-only, the effective startup configuration (§2.1) and identity.
`tls.not_after` is the field an operator must watch: the self-signed
certificate lives one year and every phone pins it (§10.1). Never the
admin token, the trunk key, or file paths of keys.

### 5.8 Calls

**`GET /v1/admin/calls`** *(9b)* → `200 [Call]`:

```json
{
  "call_id": "…", "state": "bridged", "since": "…", "held_by": "",
  "a": {"leg": "app", "device_id": "dev-a", "user": "201", "party": {"display_name": "Matt", "uri": "sip:201@dialler"}, "codec": "opus", "srtp": true},
  "b": {"leg": "trunk", "party": {"display_name": "", "uri": "sip:100@asterisk"}, "codec": "g722", "srtp": true},
  "stats": {"a_to_b": {"packets": 12034, "lost": 3, "reordered": 0, "jitter_ms": 4.1, "last_packet_at": "…"},
            "b_to_a": {"packets": 12030, "lost": 0, "reordered": 0, "jitter_ms": 1.2, "last_packet_at": "…"}}
}
```

`state` is `ringing`, `waiting_wake`, `bridged`, `held`, `handing`
(a transfer in progress). `stats` is the relay's existing per-pump
counters, absent until the call is bridged. 9b adds the registry of live
calls the B2BUA lacks today.

**`DELETE /v1/admin/calls/{call}`** *(9b)* → `204`; 404 when gone. The
one admin action that ends a call, and it ends **that call**: BYE on
both legs, the relay stopped, exactly what a `peer-timeout` expiry does.
It exists because issue #2 was a bridged call with no media that lived
fourteen hours; the liveness work closed the cause, and this is the
operator's tool for the next one. Recorded in the event ring as
`call_end` with `reason: "admin"`.

### 5.9 Events

**`GET /v1/admin/events?since=<seq>&limit=<n>`** *(9b)* →
`200 {"next": 10234, "events": [Event]}`.

A **bounded in-memory ring** of the last **4,096** operational events,
lost on restart, no operator identity (rule 4). `since` returns events
with `seq > since` (default 0: the oldest kept); `limit` defaults to 200,
max 1,000, and here it is honoured, not reserved. A client polls with
`next`; a `since` older than the ring's oldest returns from the oldest
and sets `"truncated": true`.

```json
{"seq": 10233, "at": "…", "kind": "line_state", "device_id": "dev_7K3M9Q", "user": "204",
 "detail": {"state": "refused", "error": "403 Forbidden", "realm": "asterisk"}}
```

| `kind` | `detail` |
|---|---|
| `presence` | `client` (`app`/`extension`), `online` |
| `sip_register`, `sip_unregister` | `contact`, `expires_at` |
| `line_state` | `state`, `error`, `realm` |
| `trunk_state` | `qualify` (`up`/`down`), `error` |
| `call_start`, `call_end` | `call_id`, `a`, `b` (uri), `reason` on end (`bye`, `timeout`, `peer_gone`, `admin`, `failed`) |
| `wake_sent`, `wake_ack` | `call_id`, `action` |
| `code_issued`, `code_claimed`, `code_cancelled` | — |
| `device_created`, `device_revoked`, `device_purged`, `label_changed` | — |
| `config_changed` | `version` |
| `line_changed`, `line_removed` | — |
| `directory_changed` | `version`, `added`, `changed`, `removed`, `by` (`admin`/`device`) |
| `log_level` | `level`, `sip_trace`, `until` |

Everything here is already a log line today; the ring makes it a read.
It replaces no logging.

### 5.10 Log level

**`GET /v1/admin/log`** *(9b)* →
`200 {"level": "info", "sip_trace": false, "until": null, "startup": {"level": "info", "sip_trace": false}}`.

**`PUT /v1/admin/log`** — body ≤ 1 KiB
`{"level"?: "debug", "sip_trace"?: true, "for_seconds"?: 600}` → the same
view. Fields absent are unchanged. `for_seconds` (max 86,400) reverts
both to the startup values when it elapses; absent means until restart.
Not persisted. *Effects:* the process's log level and the SIP trace
global; nothing else. This is the second and last server-wide write.

### 5.11 Diagnostics

Uploads from the phone (`POST /v1/diag`, device-authenticated, on the
device listener) land in `diag/<id>/`. The admin routes read and prune
them.

**`GET /v1/admin/devices/{id}/diag`** *(9b)* →
`200 [{"name": "20260925T085512.000Z-applog.log", "kind": "applog", "size": 48211, "at": "…"}]`,
newest first, max 1,000 entries.

**`GET /v1/admin/devices/{id}/diag/{name}`** *(9b)* → the file, with
`Content-Type` by extension (`text/plain`, `application/json`) and
`Content-Disposition: attachment`. Never follows a symlink; the name
grammar (§4.3) keeps it inside the directory.

**`DELETE /v1/admin/devices/{id}/diag/{name}`** and
**`DELETE /v1/admin/devices/{id}/diag`** *(9b)* → `204`.

**Retention:** `-diag-retain` (default 30 days). A sweeper on the server
deletes older files once an hour, per device, and logs a count. Today
nothing prunes and one dev device holds about 490 files.

### 5.12 Route table

| Method | Path | Today | Body limit |
|---|---|---|---|
| GET | `/healthz` | yes | — |
| GET | `/v1/admin/whoami` | 9b | — |
| GET | `/v1/admin/server` | 9b | — |
| GET | `/v1/admin/status` | 9b | — |
| GET | `/v1/admin/events` | 9b | — |
| GET, PUT | `/v1/admin/log` | 9b | 1 KiB |
| GET | `/v1/admin/calls` | 9b | — |
| DELETE | `/v1/admin/calls/{call}` | 9b | — |
| GET, POST | `/v1/admin/devices` | yes | 4 KiB |
| GET, PATCH, DELETE | `/v1/admin/devices/{id}` | GET/PATCH 9b; DELETE yes, `?purge=1` 9b | 4 KiB |
| POST, DELETE | `/v1/admin/devices/{id}/enrol-code` | POST yes; DELETE 9b | — |
| GET, PUT, POST | `/v1/admin/devices/{id}/config` | yes | 16 KiB |
| GET, PUT, POST, DELETE | `/v1/admin/devices/{id}/pbx-line` | yes | 4 KiB |
| GET, PUT, POST | `/v1/admin/devices/{id}/directory` | yes; `?dry_run=1` 9b | 4 MiB / 64 KiB |
| PUT, DELETE | `/v1/admin/devices/{id}/directory/{cid}` | yes | 64 KiB |
| GET, DELETE | `/v1/admin/devices/{id}/diag` | 9b | — |
| GET, DELETE | `/v1/admin/devices/{id}/diag/{name}` | 9b | — |

Device-facing routes (`/v1/directory…`, `/v1/diag`, `/v1/enrol`) are
unchanged by this document.

## 6. Wire and persistence

### 6.1 Wire protocol

**No new message and no bump.** The admin actions push what they push
today: `config`, `directory_changed`, `error/unauthorized` (fatal). One
server-side change: the gateway **keeps** `hello.app_version` and the
connection's start time and remote address per session, which it parses
and discards today, so `GET /v1/admin/status` can show them. Nothing
the app sends or receives changes.

### 6.2 Persistence

- **Schema version on each file** (SPEC 9b). `devices.json` becomes
  `{"schema": 1, "devices": {…}}`; a file whose top level has no
  `schema` is read as the current shape and rewritten with one on its
  first save. `directories/<id>.json` gains `"schema": 1` beside
  `version` and `contacts`. A file with a *higher* schema than the binary
  knows is a fatal startup error naming the file, never a silent misread.
- **`updated_at`** on each device record, set on every write to the
  entry, for `If-Match` (§4.5).
- **Atomic writes, one writer**: temp file plus rename under the store's
  mutex, as today; stated so it is not lost.
- **Round trip**: whatever is written reads back identical. 9b's fuzzer
  checks this on every store.
- **Backup** is a file copy of `-data-dir` taken at any time: every file
  is written atomically, so any instant copy is per-file consistent, and
  the three things that must be kept together are `devices.json`,
  `pbx.key` (without it every line secret is unreadable) and `tls/`
  (without it every phone must re-enrol). There is no backup API; the
  operator's runbook says `tar` the directory. Restore is stop, replace,
  start.

## 7. Harness

What 9b changes so `make harness-test` keeps passing and gains the
checks SPEC 9b names:

- `docker-compose.yml`: add `-admin-addr=:8081` and publish it
  (`${DIALLER_ADMIN_HOSTPORT:-8081}:8081`); the device listener stays on
  8080 and no longer serves `/v1/admin/`.
- `provision.sh`, `set_pbx_line.sh`, `wake_test.sh`, the README and
  `asterisk-native/README.md`: `DIALLER_API` for admin calls becomes the
  admin port. `provision.sh` line 73 (re-POST of an enrolled dev-a) becomes
  `POST /v1/admin/devices/dev-a/enrol-code`, which is what it wanted.
- Log strings the harness greps (`sip register`, `pbx line: registered`,
  `trunk: not answering OPTIONS`, and the rest) are unchanged; the event
  ring is in addition to them.
- New checks: with a dev-ha ↔ dev-hb call bridged, revoke dev-s, upload a
  CSV-shaped replace-all to dev-a, mint a code for dev-a, set the log
  level, read status, events and calls; the call's audio continues and
  neither harness phone sees a session close, a re-INVITE or a
  `directory_changed`. Then `DELETE /v1/admin/calls/{call}` and both
  phones see a BYE.

## 8. Decisions this document settles

Each was an open question in SPEC 9a, or found while writing this.

1. **Scale (§4.7):** everything, no pagination, 500-device ceiling,
   `limit` and `cursor` reserved.
2. **Concurrency (§4.5):** optional `If-Match` on every versioned write,
   412 on mismatch, last-write-wins without it. Silent loss is a client
   that chose not to send the header.
3. **`user` is immutable.** Registry, line, welcome, HA1 and the app's
   own state key on it; changing it live would need a new wire message to
   make a connected app re-register. An extension that moves to a new
   phone is a new device plus a purge of the old, and an extension that
   changes on the same phone is a purge and re-enrol. The API refuses it
   with 409 `immutable` rather than half-doing it.
4. **Revoked is an authentication state, not a deletion.** Every admin
   route works on a revoked device (§5.1).
5. **Re-POST semantics** (§5.1): existing id without token is 409;
   existing id with token replaces only the credential.
6. **Startup configuration stays in flags** (rule 3); the only
   server-wide writes are the log level and ending one call.
7. **Server unreachable, session expiry, bulk actions** are client
   behaviours and are specified in §9 rather than in the API, which
   needs nothing for them beyond what is here.
8. **Purge deletes diagnostics too** — they are the device's data.
9. **An identical settings write is a no-op**, aligned with the
   directory.
10. **Hold music is a build**, not a setting.

## 9. What 9c must do with this (recorded, not designed)

The UI is out of scope, but the API was shaped by these answers and they
are recorded so 9c does not reopen them.

- **Server unreachable.** On any request failure other than a 4xx,
  every page shows one banner ("The call server is not answering") and
  disables every write control; reads show what was last fetched with
  its `at` time. A write that fails keeps the form filled and shows the
  server's `message`. `whoami` at start distinguishes a bad token from a
  dead server. Nothing is retried automatically except reads.
- **Session lifetime.** One operator password; a session cookie of
  twelve hours absolute, refreshed on use; a form submitted after expiry
  is answered with the login page carrying the submitted values, and
  re-submitted after login, so nothing typed is lost.
- **Bulk actions.** "Copy directory to…" and any other action over N
  devices runs one request per device, in order, and reports a per-device
  result list; a failure does not stop the rest and there is no rollback.
  The operator sees exactly which devices got the copy.
- **Concurrency.** Every edit form carries the version it was loaded
  with and sends `If-Match`; a 412 is shown as "changed by someone else
  since you opened this" with a reload, never a silent overwrite.
- **CSV** (SPEC §4.8 format) is parsed and produced by `dialler-admin`,
  previewed through `?dry_run=1`, and applied as one replace-all.
- **Pages are not fixed at three.** The API supports at least: a fleet
  view (status merged with devices), a device view (record, settings,
  line, directory, diagnostics), a server view (`server`, events, log
  level), and a calls view. How they are arranged is 9c's.

## 10. Recorded, not done

1. **Certificate rotation strands every phone.** Every enrolled app
   pins the server certificate's SHA-256, and the self-signed certificate
   expires after one year. Today the only recovery is re-enrolling every
   phone. `tls.not_after` in §5.7 makes the date visible; a rollover
   (advertising the next fingerprint in `config` ahead of the switch) is
   a wire addition and a separate item. Until then the runbook is:
   deploy a long-lived `-tls-cert` before the first phone enrols.
2. **Mutual TLS** between `dialler-admin` and the call server: an option
   (SPEC 9b), not work.
3. **Live changes to trunk codecs and SRTP** are possible per call but are
   a site's security posture, so they stay flags. If a deployment ever
   needs them live, they become fields of a `PUT /v1/admin/trunk` with the
   same non-persisted, revert-on-restart semantics as the log level.
4. **A `dialler-server` `-config-file`** as an alternative to a long flag
   line is a convenience, not a contract change; the API would show the
   same effective values.
5. **Custom hold music** is a build.
