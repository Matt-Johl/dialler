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

**Call waiting.** Two calls can be up at once, one active and one on hold.
A second incoming call rings through CallKit's own UI — on iOS 26 that is
the ordinary Accept / Decline, and Accept holds the current call before
answering (the app receives `CXSetHeldCallAction` + `CXAnswerCallAction`
in one transaction); "End & Accept" is shown only when holding is not
allowed, which for two calls of one app means `maximumCallGroups < 2`
(SPEC §4.4 rule 8 has the rule and where it was read from). While a call
is held, iOS 26 shows its own **Swap** banner over the app, so the in-call
screen adds no control of its own: it names the held party under the
active one and hides Hold; on iOS 17/18, which show no banner, Hold reads
Swap. When the call in progress ends and the only call left is on hold,
the controller asks CallKit to resume it, so the user is back in that
call without a tap. The app plays the call-waiting beep (iOS plays none
for VoIP apps).
A held call owns no
audio units: the shim's hold stops the call's audio and keeps baresip from
re-creating its source, because iOS allows one VoiceProcessingIO input and
the active call must have it (SPEC §4.4 rule 8).
Every layer addresses calls by
id: baresip's SIP Call-ID in the shim and engine (`cb_event_info`,
`answer(engineCallID:)` …), the server's `X-Dialler-Call-ID` header to pair
an INVITE with its wake, and the controller's own ids for CallKit. A third
caller, or any second caller while Settings › Call waiting is off, gets
486 and hears busy. `make sim-call-cw` is the headless twin of the device
checklist (SPEC §7.3 item 5).

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

### Audio routes: earpiece, speaker, Bluetooth, CarPlay

CallKit owns the audio session; the app only configures it
(`CallKitBridge.configureAudioSession`: `.playAndRecord`, mode `.voiceChat`,
`.allowBluetoothHFP`, preferred 48 kHz / 20 ms) and hands activation to the
engine (`didActivate` releases baresip's audiounit units, `didDeactivate`
holds them). Everything below follows from that.

- **Routes are CoreAudio's business, not ours.** Earpiece ↔ speaker
  (`AppModel.toggleSpeaker` → `overrideOutputAudioPort`), a Bluetooth headset
  appearing or dropping, CarPlay: the VoiceProcessingIO unit keeps running
  and CoreAudio reconfigures its hardware rate underneath it — the
  `audiounit: record: enable resampler 16000.0 -> 16000 Hz` line on a
  Bluetooth route is that. The app logs each change
  (`callkit: audio route change reason=N now <route>`; reasons: 1 new
  device, 2 device gone, 3 category change, 4 override, 8 configuration
  change) and does nothing else. Only an *interruption* (a cellular call,
  Siri) stops audio: CallKit deactivates the session, the units are held,
  and a new activation restarts them — `make audio-probe` scenarios C and D
  exercise exactly that restart, D under rapid churn.
- **Bluetooth is HFP only.** HFP (hands-free profile) is the one Bluetooth
  profile that carries a microphone; A2DP is playback-only and never used
  for a call. Echo cancellation on an HFP route is the headset's; VPIO's own
  AEC stays harmless.
- **Wideband survives HFP, latency does not.** Modern headsets negotiate
  mSBC (16 kHz), so a G.722 or Opus call stays wideband end to end; an old
  headset falls back to CVSD (8 kHz) and the call sounds narrowband
  regardless of the SIP codec. The HFP link itself adds latency the app
  cannot see or reduce: eSCO slots and mSBC framing are 7.5 ms, but the
  headset's and the phone's Bluetooth audio buffers put a typical headset
  at 50–100 ms one way, AirPods at the low end of that (the 150–250 ms
  figures often quoted for Bluetooth are A2DP music streaming and do not
  apply to calls). So the mouth-to-ear targets in SPEC §7.2 (≤ 150 ms LAN
  echo, ≤ 200 ms through the PBX) are measured on the built-in earpiece
  and speaker; on a headset the same call reads 100–200 ms higher on the
  echo test, and that difference is the link, not the app. To see it:
  call `echo` on the earpiece, then on the headset, and compare the delay
  of your own voice coming back.
- **Device checklist (SPEC §7.3 item 3):** during one call, switch
  earpiece → speaker → earpiece; connect a Bluetooth headset mid-call and
  disconnect it; start the call on the headset; take the call on CarPlay;
  let a cellular call interrupt and end. Audio must continue (or resume
  after the interruption) in every case, with a route-change line and no
  `audiounit` error in the log.

For a device call the server must advertise an address the phone can reach.
`make harness-up` now defaults `DIALLER_PUBLIC_HOST` to this Mac's en0
address and prints it; override with `DIALLER_PUBLIC_HOST=... make harness-up`.
The server's media ports (UDP 20000–20100) are published on the host, so both
the phone and the docker phones reach the relay at that address. The wake's
SIP target must show that address in the app log ("registering 201@dialler
to 10.x.x.x:5061"); if it says `dialler`, the server was started without it.

### Before shipping: export compliance and attribution

The app links OpenSSL, so every App Store submission has to answer the
encryption question. Set `ITSAppUsesNonExemptEncryption` in `Info.plist` to
avoid being asked on each upload. Using TLS/SRTP for a VoIP app's own calls
normally falls under the standard exemption for encryption limited to
authentication and secure communication, in which case the key is `false`;
confirm that against Apple's current wording, and if the exemption does not
apply a self-classification report (and, historically, a French declaration)
may be required. This is a distribution question, not a licence one.

Separately, the vendored stack is all permissive — OpenSSL 3.3 is Apache-2.0,
Opus 1.5, libre and baresip 3.15 are BSD-3 (verified from the licence files in
`ios/vendor/src`, SPEC §8) — but binary redistribution under both licences
still requires reproducing the copyright notices, licence texts and warranty
disclaimers. The app has no acknowledgements screen yet; add one, or bundle a
licence file, before release. The BSD no-endorsement clause also means the
baresip/Xiph/contributor names must not be used to promote the app.

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

Settings → "Enable background wakeups on these SSIDs" (comma-separated;
the provider runs on any of them) configures
`NEAppPushManager` for the extension. Requires the
`app-push-provider` Network Extension entitlement on the provisioning profile
(SPEC §9 risk 1). Not available in the simulator.

## Building from a restricted sandbox

`swift test` in these packages needs `--disable-sandbox` and redirected module
caches (see `make swift-test-sandboxed`). `xcodebuild` additionally needs the
`CLANG_MODULE_CACHE_PATH` / `SWIFTPM_MODULECACHE_OVERRIDE` environment and a
`-derivedDataPath` under `~/Library/Developer`.

## When the app dies or freezes: what to collect

First: **was the app launched from Xcode?** A debugged process that stops
(breakpoint, signal, CPU-limit exception) has its task suspended: it cannot
take a CallKit wake, callservicesd's kill "succeeds" without the process
exiting, a tap shows the frozen UI, swipe-kills are accepted and never
complete, and it stays until the debugger lets go or the phone reboots.
Nothing of ours logs and no crash report is written. Three field incidents
(2026-09-12/13) were this. For device testing install with Xcode, stop,
and launch from the home screen — or untick "Debug executable" in the
scheme's Run action. In a device log archive the tell is `debug:asserted`
on the app's process state, or FrontBoard's "process is being debugged".

Most of the rest now collects itself:

- The app and the extension keep a persistent log in the App Group
  (`logs/app.log`, `logs/extension.log`, rotating at 2 MB), with every line
  the in-app log view shows, including the SIP engine's.
- On every launch, and from the Log section's **Send diagnostics** button,
  the app uploads those logs plus any MetricKit report iOS handed it
  (crash, hang, CPU kill — delivered on the launch after the event) to the
  dev server, `POST /v1/diag`, which files them under
  `data/diag/<device>/<time>-<kind>…`. Anything the server did not accept
  stays queued in the App Group for the next attempt.
- `make dev-server` also writes its own log to `data/logs/dev-server.log`.
- Every Xcode build keeps its symbols in `ios/dsyms/<UUID>/` (the "Keep
  build symbols" phase; the project has `ENABLE_USER_SCRIPT_SANDBOXING`
  off because Xcode's script sandbox would silently refuse that write),
  and `make symbolicate FILE=<report>` resolves a `.ips` or a MetricKit
  JSON against them.

So after an incident: rebuild nothing, open the app once (or press Send
diagnostics), and read `data/diag/` and `data/logs/` on the Mac. The manual
route still exists for what MetricKit does not cover: Settings → Privacy &
Security → Analytics & Improvements → Analytics Data for `Dialler…ips`
files, and the device console for the same minute
(`log show --last 5m --predicate 'process CONTAINS "Dialler"' --info
--style compact`).

- **`Dialler-…ips` with `SIGKILL` and FrontBoard `0xBAADCA11` from
  callservicesd** — a VoIP push that was not reported to CallKit in time.
  Console signatures: `CSDVoIPProcessAssertion` granted, then the kill
  ~7 s later with no `handleExtensionWake` in between. Fixed 2026-09-11
  (`LocalPushDelegate`); SPEC §9 item 7.
- **`Dialler.cpu_resource_fatal-…ips`** ("cpu usage", 99 % over ~48 s,
  process killed, heaviest stack the loop thread inside `re_main`) — the
  SIP engine's poll loop spinning on `EBADF`, i.e. its kqueue descriptor
  went bad. Since 2026-09-12 the loop gives up after eight retries and
  the engine rebuilds the stack, so the app should recover instead of
  freezing; the *cause* of the bad descriptor is still open. Collect the
  app log lines containing `fd_poll EBADF` — the `kqfd=` value in them is
  the deciding clue (`-1`: libre's context was torn down under the loop;
  `>=0`: another component closed a descriptor number it did not own) —
  and `re_main returned … unasked` from the shim, plus what the phone had
  just done (unlock, call end, Wi-Fi change). Full write-up: SPEC §9
  item 8.
