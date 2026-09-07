# iOS

Three pieces, layered so almost everything is testable without a device
(SPEC §7):

| Path | What | Tested by |
|---|---|---|
| `DiallerProtocol/` | Wire envelope, bodies, framing. Shared golden fixtures with the Go server. | `swift test` (golden round-trip) |
| `DiallerCore/` | `LANSocketTransport` (Network.framework TLS), `SessionMachine` (handshake, heartbeat, wake de-dup), `CallController` (wake → system call UI → acks; answer → engine), config store (App Group + keychain), directory client + address book. Apple-specific pieces sit behind `CallUI` / `CallEngine` / `SignalTransport` protocols with fakes. | `swift test` on macOS, no device |
| `Dialler/` | Xcode project: SwiftUI app (`App/`) and the `NEAppPushProvider` extension (`PushProvider/`). Both link `DiallerCore`. | simulator (app), device (extension) |

## Identifiers

Bundle prefix `com.latentbadger.dialler`, App Group `group.com.latentbadger.dialler`.
They appear in `DiallerCore/Sources/DiallerCore/Identifiers.swift`, the two
`.entitlements` files, and the `PRODUCT_BUNDLE_IDENTIFIER` build settings.
Set `DEVELOPMENT_TEAM` in the project (or Xcode's Signing tab) before a
device build; the simulator needs no team.

## How a call reaches the screen

- **Foreground (registered):** the gateway's `welcome` names the device's
  SIP account, so the app registers its user agent as soon as it connects
  and stays registered while it runs. An incoming call then arrives as a SIP
  INVITE at `BaresipCallEngine`, which reports it to `CallController`;
  the controller rings CallKit from the INVITE. The server sends a `wake` as
  well; whichever arrives first rings, and the controller treats the pair as
  one call. On answer the engine answers the pending INVITE immediately.
- **Foreground (not yet registered):** the app's `LANSocketTransport`
  (kind `app`) receives `wake` → `CallController` →
  `CallKitBridge.reportIncoming` → CallKit rings → `wake_ack(will_answer)`
  goes back on the same socket; on answer the engine registers on demand and
  answers the INVITE the server then sends.
- **Background / killed, on the office SSID (device only):** the extension's
  `LANSocketTransport` (kind `extension`) receives `wake` →
  `NEAppPushProvider.reportIncomingCall(userInfo:)` → iOS launches the app and
  delivers it through `PKPushRegistry` → the same `CallController` → CallKit.
  The extension sends the `wake_ack`.
- Both paths are de-duplicated on `call_id` in the controller, so a wake that
  arrives on both sockets rings once.

On answer, `CallEngine.prepareForIncomingCall` is invoked with the callee's
SIP user and the SIP target from the wake. `DiallerEngine/BaresipCallEngine`
then starts libbaresip, registers `user@domain` to the target over TLS with
the server as outbound proxy, and answers the INVITE the server bridges to
it. `LoggingCallEngine` is the fallback when the engine is not linked.

## SIP engine (`DiallerEngine/`)

The C stack is cross-compiled by `ios/vendor/build-baresip.sh` (run it once:
`make ios-vendor`, ~10 minutes) into `ios/vendor/xcframeworks/` — libre and
baresip 3.15 with static modules `audiounit`, `opus`, `g711`, `ice`, `srtp`,
Opus 1.5, OpenSSL 3.3 — for arm64 device and arm64 simulator. They are not
committed. `Sources/CBaresip` is a small C shim owning the libre thread;
`BaresipCallEngine` is the Swift `CallEngine` on top.

For a device call the server must advertise an address the phone can reach.
`make harness-up` now defaults `DIALLER_PUBLIC_HOST` to this Mac's en0
address and prints it; override with `DIALLER_PUBLIC_HOST=... make harness-up`.
The server's media ports (UDP 20000–20100) are published on the host, so both
the phone and the docker phones reach the relay at that address. The wake's
SIP target must show that address in the app log ("registering 201@dialler
to 10.x.x.x:5061"); if it says `dialler`, the server was started without it.

### Headless engine test (no phone)

The same stack builds for macOS, and `engine-probe` drives the real
`BaresipCallEngine` from a Mac shell against the docker server — the
automated sibling of the on-device answer path (SPEC §7.1). It registers 201
and answers a call if one arrives, printing baresip's own log.

```sh
make ios-vendor                                            # once; includes the macOS slice
DIALLER_PUBLIC_HOST=$(ipconfig getifaddr en0) make harness-up
make engine-probe HOST=$(ipconfig getifaddr en0)           # expect "probe: REGISTERED"
make harness-ring-sim                                      # in another shell: the probe answers, media flows
```

If the phone gets stuck in "registering", run this first: it fails the same
way with the reason in plain text, without a device in the loop.

Audio: the CallKit bridge sets the `playAndRecord` / `voiceChat` category when
it reports the call and CallKit activates the session on answer; baresip's
`audiounit` module starts its units when the call is established, which is
after REGISTER and INVITE and therefore after activation.

## Running against the harness in the simulator

1. `make harness-up` — server on `127.0.0.1:7443`, provisioned; note the
   `dev-a` token it prints.
2. Open `Dialler/Dialler.xcodeproj`, run the `Dialler` scheme on an iPhone
   simulator.
3. Settings tab: host `127.0.0.1`, port `7443`, keep "accept self-signed",
   device ID `dev-a`, paste the token, **Save & connect**. Status shows
   `connected` and the directory tab fills from `/v1/directory`.
4. Ring it: `make harness-ring-sim` makes the harness phone 202 dial 201.
   Nothing else holds 201's registration, so the server wakes `dev-a`, the
   app, over its socket and the simulator shows the CallKit incoming-call
   screen. Answering logs the SIP target the engine would register to;
   declining sends `wake_ack(decline)`; the server times out the caller with
   480 either way until the SIP engine exists.

`make ios-test` runs the package tests; `make ios-typecheck` compiles the
packages for the simulator and type-checks the app and extension sources
without Xcode's package resolution (both pass as of 2026-09-05);
`make ios-build` is the Xcode build.

## Running on a real device

Same as the simulator, but use the Mac's LAN address (e.g. `ipconfig getifaddr
en0`) as the host: the harness publishes `7443`, `5061` and `8080` on all
interfaces. The device must be on the same Wi-Fi, and iOS will ask for Local
Network permission the first time. Set `DEVELOPMENT_TEAM` (Signing tab) on
both targets. For the later SIP step, start the server with
`-public-host <mac-ip>` so the wake's SIP target is something the device can
resolve; today it is `dialler`, which only matters once the engine exists.

## Local Push Connectivity (device only)

Settings → "Enable background wakeups on this SSID" configures
`NEAppPushManager` for the extension. Requires the
`app-push-provider` Network Extension entitlement on the provisioning profile
(SPEC §9 risk 1). Not available in the simulator.

## Building from a restricted sandbox

`swift test` in these packages needs `--disable-sandbox` and redirected module
caches (see `make swift-test-sandboxed`). `xcodebuild` additionally needs the
`CLANG_MODULE_CACHE_PATH` / `SWIFTPM_MODULECACHE_OVERRIDE` environment and a
`-derivedDataPath` under `~/Library/Developer`.
