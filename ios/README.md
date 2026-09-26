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

**Directory (SPEC §6 item 7).** Each device has its own directory on the
server, and the server is the source of truth: the Directory tab's add,
edit, delete and star all write there first (`DirectoryClient.create /
update / delete`, device-authenticated, online only) and the list follows
by the usual delta sync, which the server's `directory_changed` to this
device triggers and the app also requests after each write. The star flips
optimistically and a failed write puts it back. The book is persisted in
`<App Group>/directory/book.json` (`AddressBookStore`), so the cursor
survives a launch and the list is on screen before the first sync. A
server whose version is *behind* that cursor has been reset (a
re-provisioned harness; the move to per-device directories, whose
counters start at 1): `DirectoryClient.sync` clears the book and syncs
from zero, and the log says so. Found on 2026-09-21 the hard way — the
app held the old global cursor 702, every write landed on the server,
and none of them showed.
Search (`DirectorySearch`) matches any substring of the name or the
extension, case- and diacritic-insensitive, locally. A new contact's mode
is guessed from its number (bare or in our SIP domain → local, elsewhere →
trunk) and can be changed.

**Recents (SPEC §6 item 6).** Every way a call leaves the controller's
table emits one `CallRecord` through `CallController.onCallEnded`
(`DiallerCore/Recents.swift`: direction, the other party, start / connect /
end times, and an outcome from the classification table there — completed,
missed, declined, cancelled, answered elsewhere, or the same failure the
in-call screen names). The app writes them to
`<App Group>/recents/recents.json` through `RecentsStore` (newest first,
500 kept); the extension never touches that file — for each wake it reports
it drops `recents/pending/<call id>.json`, which the app folds in at
launch, on foreground and at each call end, discarding any it has its own
record for. A call the app never ran for therefore still shows as missed.
The controller also logs each record as `recents: <outcome> <direction>
<uri> <duration>`, which `harness/sim_call.sh` asserts per mode. The
Recents tab (first in the tab bar, badged with unseen missed calls) lists
them with All / Missed, tap to redial, swipe to delete.
Every layer addresses calls by
id: baresip's SIP Call-ID in the shim and engine (`cb_event_info`,
`answer(engineCallID:)` …), the server's `X-Dialler-Call-ID` header to pair
an INVITE with its wake, and the controller's own ids for CallKit. A third
caller, or any second caller while Settings › Call waiting is off, gets
486 and hears busy. `make sim-call-cw` is the headless twin of the device
checklist (SPEC §7.3 item 5).

**Hold music** is not the app's (plan Phase K). When the app holds a call
the server plays music to the other party — the app cannot, because a held
call owns no audio units and the one VoiceProcessingIO belongs to the
active call (SPEC §4.4 rules 8 and 8b). The app signals hold and nothing
else; what the held party hears is `server/internal/moh`.

**Call-progress tones** (plan Phase J). iOS plays none of them for a VoIP
app, and neither does a B2BUA that answers with a bare 180, so the app
makes its own: `CallTones` (DiallerCore) renders 425 Hz ETSI cadences to
PCM and `TonePlayer` (the app) plays them with an `AVAudioPlayer` on the
session CallKit has already activated for the call, mixing with baresip's
audio unit. Which tone and when is the *controller's* decision, because it
is a SIP question — it reaches the app through `CallUI.playTone`:

| Tone | When |
|---|---|
| ring-back, 1 s / 4 s | our outgoing call is alerted by a 180. Not on a 183: early media is the far end's own ring-back and ours would double it. Stops on answer, on the end, and if another call is answered first. |
| busy, 0.5 s / 0.5 s | the call was refused 486 / 600 / 603. |
| congestion, 0.25 s / 0.25 s | any other refusal — 480 is what our own server returns when nothing can be woken. |
| call waiting, two 100 ms bursts every 5 s | a second call rings behind one in progress. Decided in `CallKitBridge`, not the controller: it is CallKit's view of the calls that makes a call "waiting". |

**A tone never activates the session.** It belongs to CallKit, which hands
it over at `didActivate` — but ring-back is asked for on the 180, which
arrives first, and `AVAudioPlayer.play()` would activate the session itself.
`TonePolicy` (DiallerCore) holds such a tone until activation and drops it
at deactivation; `TonePlayer` only does what it says. The rule sits in
DiallerCore because the app target has no tests, and it shipped broken once
(2026-09-15) while it lived in the player.

`OUTBOUND=212 make sim-call` and `make sim-call-refused` are the headless
twins (both in `sim-call-all`); what a device is still needed for is in
SPEC §7.3 item 6.

A failure tone delays the *report* of the end by its own length (4 s busy,
3 s congestion), because CallKit takes the audio session away with the
call and a tone started after the end report is cut off. Hanging up on it,
or a new call arriving, ends the call there and then. Nothing is played
for a call that merely ended (a BYE, status 0) or for the 487 answering
the user's own cancel. The SIP status comes up from the shim
(`cb_event_info.scode`, `call_scode()`), not from parsing reason text.

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
3. A fresh install opens on the setup screen (SPEC §6 item 8). Two ways in:
   - **Enter details manually**: server `127.0.0.1`, port `8080`, and the
     eight-character `code` the harness printed for `dev-a` (it is valid
     for fifteen minutes; `make harness-up` again for a fresh one). The
     app claims it, pins the server's certificate, and connects. Note the
     claim rotates dev-a's token, so the fixed fixture no longer works
     for that device until the harness is re-provisioned.
   - **Scan QR code** needs a camera, so not in the simulator (the button
     is disabled there); on a device, render the printed `url` as a QR,
     or open it from the Camera app — the `dialler://` scheme launches
     the app.
   - The dev path, for the fixed tokens: press and hold the setup screen's
     icon for five seconds to open the Status page, and in "Connection
     (dev)" enter host `127.0.0.1`, port `7443`, device ID `dev-a`, the
     fixed token, keep "accept any certificate", **Save & connect**.
   Once enrolled the Status page has no tab: press and hold the
   **Settings** title for five seconds and it is pushed; Back or any
   tab-bar tap closes it. It shows `connected`, and the directory tab
   fills from `/v1/directory`. Settings › **Log out** (after an "are you
   sure") clears the credential, removes the Local Push configuration so
   the extension stops and the server sees the phone go offline, and
   returns to setup.
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

The office SSIDs the provider runs on are **set by the administrator per
device** (SPEC §6 item 8b): they arrive in the welcome (and as a `config`
push on change) and the app configures `NEAppPushManager` itself
(`LocalPushPolicy` decides; an identical list is never re-saved, because
that can restart the provider). Settings shows the list read-only. A server
that has no settings for the device leaves the phone's own configuration
alone; for that case, and for clearing a stale one, the Status page has
"Local Push (dev)" with the old typed field. Requires the `app-push-provider`
Network Extension entitlement on the provisioning profile (SPEC §9 risk 1).
Not available in the simulator.

## Building from a restricted sandbox

`swift test` in these packages needs `--disable-sandbox` and redirected module
caches — `make` now sets `SWIFTPM_MODULECACHE_OVERRIDE` and
`CLANG_MODULE_CACHE_PATH` itself, because without them even a package
*manifest* will not compile. Since the Xcode that ships Swift 6.4,
`make ios-typecheck` also cannot expand SwiftUI macros in the sandbox: the
`swift-plugin-server` returns a malformed response, so any view using
`@State` reports that and a cascade of "cannot find '$binding'". Non-view
sources still typecheck, and the real gate is `make ios-build`
(`xcodebuild`); an Xcode update also changes SwiftPM's scratch layout, which
is why the module search paths in the Makefile list several. `xcodebuild` additionally needs the
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
- **`closed (Connection reset by peer)` is NOT a network failure.** libre
  answers a received BYE with 200 OK and then terminates the session with
  `ECONNRESET`, which baresip prints through `strerror`; "Connection reset
  by user" is the local hangup. Both are ordinary. Logs written from
  2026-09-15 say `far end hung up (BYE)` / `hung up here` and keep the raw
  text in brackets, but older logs, and baresip's own lines, still carry
  the bare errno. Two days went into chasing it (SPEC §9 item 10); check
  the extension log for `wake cancelled … caller_hangup` at the same
  moment before suspecting anything else.
- **`gateway: session down (<reason>); N call(s) tracked, app <state>`** —
  new on 2026-09-15, and before that a gateway drop left no trace at all.
  With `N` greater than zero the session that carries `wake_cancel` is
  gone while calls are up, which is how a phone ends up ringing after the
  caller has given up. Its counterpart, `gateway: staying connected in the
  background (N call(s) tracked)`, says the app correctly refused to let go.
- **Cross-log timing.** Align the app log against
  `data/logs/dev-server.log` by *events* (a call id appears in both), never
  by timestamps: on 2026-09-15 the phone's clock ran 37 s ahead of the
  Mac's, enough to make an effect look like a cause.
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
