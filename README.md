# Dialler

Native iOS SIP softphone with on-prem wakeups (no APNS) and an on-prem light
server that is its own exchange. [SPEC.md](SPEC.md) is the source of truth for
architecture and phases; this file is the map of the repo.

| Path | What | Status |
|---|---|---|
| [`protocol/`](protocol/) | Frozen v1 wire protocol ([PROTOCOL.md](protocol/PROTOCOL.md)) and golden fixtures shared by Go and Swift | Phase 0 ✔ |
| [`server/`](server/) | Go light server: signal gateway, enrolment, registry, routing, directory API, and the **sipgo + diago** app-leg SIP element (registrar, wake-then-bridge call controller, media proxied through the server) | Phase 1 server half ✔ |
| [`ios/DiallerProtocol/`](ios/DiallerProtocol/) | Swift package: envelope, bodies, framing, shared golden fixtures with Go | Phase 0 ✔ |
| [`ios/DiallerCore/`](ios/DiallerCore/) | Swift package: TLS signal transport, session machine, call controller, config store, directory client, all behind fakes | Phase 1 ✔ (12 tests) |
| [`ios/Dialler/`](ios/Dialler/) | Xcode project: SwiftUI app + CallKit + PushKit, and the `NEAppPushProvider` extension | rings on device, Local Push saves |
| [`ios/DiallerEngine/`](ios/DiallerEngine/) | baresip-backed `CallEngine` over XCFrameworks built by [`ios/vendor/build-baresip.sh`](ios/vendor/build-baresip.sh) | builds for device + simulator; first real call pending |
| [`harness/`](harness/) | Docker: Asterisk peer, SIPp conformance, headless baresip phones, end-to-end call test | passing |
| [`spike/`](spike/) | Standalone diago B2BUA from the Phase 0 engine spike | superseded by `server/internal/b2bua`; delete once the repo is committed |

## Run the tests

```sh
make test          # Go (vet + test) and Swift
make go-test
make swift-test    # or swift-test-sandboxed inside a restricted build sandbox
```

Every Go test except the UDP relay runs socket-free (in-memory pipes and
`ServeHTTP`), so the suite passes inside sandboxes that forbid binding. The
relay tests skip with an explicit message when UDP bind is forbidden; run
them on a normal machine to exercise real forwarding.

## Run the server locally

```sh
make run           # self-signed TLS on :7443 (signal), :5061 (SIP) and 127.0.0.1:8080 (directory/admin)
```

Enrol a device, give it a contact, and fetch its directory (`-k`: the dev
certificate is self-signed). The token doubles as the SIP Digest password on
the app leg, with the device id as the username. Directories are **per
device** (SPEC §6 item 7): the admin writes `/v1/admin/devices/<id>/directory`
(`PUT` with `{"contacts":[…]}` replaces the whole list, as a CSV upload
does), and the device reads and writes its own at `/v1/directory`:

```sh
curl -sk -H 'Authorization: Bearer dev' -H 'Content-Type: application/json' \
     -d '{"device_id":"dev-a","user":"201"}' https://127.0.0.1:8080/v1/admin/devices
curl -sk -H 'Authorization: Bearer dev' -H 'Content-Type: application/json' \
     -d '{"display_name":"Desk","uri":"sip:100@asterisk","mode":"trunk"}' https://127.0.0.1:8080/v1/admin/devices/dev-a/directory
curl -sk -H 'X-Device-ID: dev-a' -H "Authorization: Bearer $TOKEN" 'https://127.0.0.1:8080/v1/directory?since=0'
```

A server that still has the old global `data/directory.json` folds it into
every enrolled device's directory at start-up, once, and renames it
`directory.json.migrated`.

Adding a device also mints its **enrolment code** (SPEC §4.8): eight
characters, fifteen minutes, single use, returned with a `dialler://enrol`
link the admin UI shows as a QR. Leave `token` out (as a real deployment
does) and the device has no credential until it claims the code; a
`token` (the harness's fixed fixtures) issues that credential at once as
well. `POST /v1/admin/devices/<id>/enrol-code` mints a new one for an
existing device — a lost phone, or a revoked device coming back. The phone
claims with the server's one unauthenticated write, which rotates the
credential and drops whatever was connected with the old one:

```sh
curl -sk -H 'Content-Type: application/json' -d '{"code":"A7K2-M9PX"}' https://<host>:8080/v1/enrol
# → {"device_id","user","token","signal_port","sip_domain","cert_sha256"}
```

A miss answers 404 after half a second, and a source address gets five
attempts a minute. `cert_sha256` (also in the link) is what the app pins.

A device's office Wi-Fi list is server-managed too (SPEC §6 item 8b):

```sh
curl -sk -H 'Authorization: Bearer dev' -H 'Content-Type: application/json' -X PUT \
     -d '{"ssids":["Office","Office-5G"]}' https://127.0.0.1:8080/v1/admin/devices/dev-a/config
```

The app receives it in its welcome and as a `config` push on every change,
and applies it to Local Push itself. Until an administrator has set a list
the phone keeps whatever it has.

## The admin UI

`dialler-admin` (SPEC §6 item 9) is the operator's web interface: a
separate process that holds the admin token and a password of its own,
speaks only to the call server's admin API, and renders plain HTML with no
scripting. Devices with their live state, add a device and show its
enrolment QR, new code, each device's office Wi-Fi list, its directory
with add and edit pages, CSV download, CSV upload (previewed, then applied
as one change) and copy-to-other-devices, and a danger zone with revoke
(the directory is kept; a new code brings the phone back) and delete
(`DELETE /v1/admin/devices/<id>?purge=1`: record, directory and settings
gone, the extension free again). The Server page holds the PBX settings
(`/v1/admin/pbx`): trunk-peer mode for Asterisk, or registered-devices
mode for CUCM, where each device's Edit page takes the extension's digest
credentials (`/v1/admin/devices/<id>/pbx`). Saved PBX settings are read at
the call server's next start in place of the `-trunk` flags; the
per-extension registration itself is the next piece of the call element
(SPEC §6 item 3) and the UI says so until it lands.

```sh
make admin
echo 'yourpassword' | ./bin/dialler-admin set-password -password-file data/admin/password
make dev-admin        # https://127.0.0.1:8443 against make dev-server
```

It verifies the call server with `-server-ca` (the server's kept
self-signed certificate, `data/tls/self-signed.pem`, works as one) and
serves its own pages over TLS with a certificate it keeps under
`data/admin/tls`. In the docker harness, `make harness-admin-up` runs it on
https://localhost:8443 with the password `harness-admin`.

### The SIP domain

Users are SIP addresses like `sip:201@dialler`; the part after the `@` is
the **SIP domain**, set with `-local-domain` (`dialler` in `make dev-server`
and in the docker harness; unset, it defaults to the server's own address).
It plays the role a domain plays in an email address, naming who owns those
identities, and need not be a DNS name. The server uses it for:

- **ownership** — a REGISTER for a user in any other domain is refused, and
  an address in a foreign domain routes to the PBX rather than to an app;
- **what apps are told** — the welcome carries each account as user plus
  domain, and the app builds its address from it;
- **the Digest realm** — app-leg authentication (SPEC §4.4 rule 2) uses the
  domain as the realm, and the server stores each device credential only
  as the MD5 of device id, realm and token.

Because the realm is baked into the stored credential, **renaming the domain
invalidates every enrolled device** until its credential is re-issued. In
development this is invisible (`harness/provision.sh` re-issues the fixed
dev credentials on every start); in production keep the domain fixed, or
re-enrol devices after a rename. There is no reason to change it.

## Server packages

```
cmd/dialler-server     wiring + flags
cmd/dialler-admin      the operator's web UI (internal/adminui), a separate process
internal/adminui       admin UI: API client, login, pages, embedded templates + stylesheet
internal/status        GET /v1/admin/status: the server and every device's live state
internal/csvdir        directory ↔ CSV
internal/qr            QR encoder for the enrolment link
internal/wire          protocol envelope, bodies, framing        (golden tests)
internal/gateway       TLS signal gateway: hello/welcome, wake fan-out, replay, supersede, idle
internal/enroll        device credentials, enrolment codes + the claim route, admin API, device-auth middleware
internal/registry      user ↔ device ↔ SIP contact location table (+ WaitRegistered for the wake path)
internal/routing       local | trunk | unknown decision
internal/b2bua         app-leg SIP element on sipgo + diago: TLS registrar, wake-then-bridge, media proxy
internal/directory     one address book per device (data/directories/<id>.json) with delta sync, favourites, replace-all + REST (device and admin)
internal/pbx           PBXAdapter seam (None only so far)
internal/tlsutil       cert loading / self-signed dev cert
internal/sip           hand-written SIP parser/registrar — fallback and reference, not on the call path
internal/relay         hand-written UDP relay — fallback and reference; diago's bridge proxies media
```

Dependencies are vendored (`server/vendor`), so builds and tests are offline.

## Headless call test

```sh
make harness-call      # 201 dials 202 through dialler-server; asserts the callee's recorded audio
make harness-wake      # callee asleep: gateway wake → fake app registers it → bridge → audio
make harness-up        # server + Asterisk, provisioned
make harness-test      # SIPp: TLS register, offline → 480, unknown → 404, unregister, plain TCP refused
```

`cmd/fake-app` is the headless stand-in for the app's background wake path:
it holds a wire-protocol connection as an enrolled device, acks the wake, and
brings the SIP user agent up (here, a baresip via `ctrl_tcp`).

## iOS

See [ios/README.md](ios/README.md). `make ios-test`, `make ios-typecheck`,
then open `ios/Dialler/Dialler.xcodeproj` and run on an iPhone simulator
against `make harness-up`; `make harness-ring-sim` rings it.

## Next

The roadmap lives in [SPEC.md §6](SPEC.md) ("Remaining near-term"). Items 6–9
(added 2026-09-21) are the next tranche, in build order: a Recents tab,
per-device directories with favourites, search and in-app editing, first-run
enrolment by QR or code with the Status tab hidden behind a gesture, and a
separate `dialler-admin` web UI (`cmd/dialler-admin`, `internal/qr`) that
manages devices and directories (CSV up/down) through the admin API. The
mechanism and the isolation rule are in SPEC §4.8: nothing the admin does
interrupts the call server or any device other than the one named.
