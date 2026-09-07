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
make run           # self-signed TLS on :7443 (signal) and :5061 (SIP), HTTP on 127.0.0.1:8080
```

Enrol a device and fetch the directory:

```sh
curl -s -H 'Authorization: Bearer dev' -H 'Content-Type: application/json' \
     -d '{"device_id":"dev-a","user":"201"}' http://127.0.0.1:8080/v1/admin/devices
curl -s -H 'X-Device-ID: dev-a' -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:8080/v1/directory?since=0'
```

## Server packages

```
cmd/dialler-server     wiring + flags
internal/wire          protocol envelope, bodies, framing        (golden tests)
internal/gateway       TLS signal gateway: hello/welcome, wake fan-out, replay, supersede, idle
internal/enroll        device credentials + admin API + device-auth middleware
internal/registry      user ↔ device ↔ SIP contact location table (+ WaitRegistered for the wake path)
internal/routing       local | trunk | unknown decision
internal/b2bua         app-leg SIP element on sipgo + diago: TLS registrar, wake-then-bridge, media proxy
internal/directory     address book with delta sync + REST
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

## Next (Phase 1, remaining)

- **First real call on a device**: `make ios-vendor` once, then build the
  app (it now links `DiallerEngine`), start the harness with
  `DIALLER_PUBLIC_HOST=$(ipconfig getifaddr en0) make harness-up`, and answer
  a `make harness-ring-sim` call. Expect registration → INVITE → established
  → audio through the server's relay.
- Killed-app wake through the Local Push extension (SPEC §7.3.1–2).
- Ring-back before the callee answers (diago answers the caller first).
- SIP Digest on the app leg before the public edge (Phase 4b).
- Trunk-side listener for the PBX leg (Phase 2).
