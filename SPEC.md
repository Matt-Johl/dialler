# Dialler — Project Specification & Architecture

> Status: planning / feasibility draft. Native iOS softphone with on-prem
> wakeups via Local Push Connectivity (no APNS), designed for test-first,
> component-by-component development.

## 1. Goal

A native iOS SIP softphone that receives incoming-call wakeups **on-premises
without Apple's Push Notification Service (APNS)**, so that calls are not missed
when the app is backgrounded/killed and even when the site has no internet
access. It interoperates with existing PBXs **and can also place calls directly
between app instances on different devices through the light server with no PBX
present**, maintains a server-synced address book, and supports hold/transfer.
Off-prem (remote) users, video and IM are not scheduled — see §6 "Much later".

## 2. Core mechanism (the load-bearing decision)

Wakeups use Apple's **Local Push Connectivity (LPC)** via the Network Extension
framework: an **App Push Provider** extension (`NEAppPushProvider`, managed by
`NEAppPushManager`) maintains a persistent TLS connection to an on-prem server
and reports incoming calls directly to **CallKit**. This is the only
Apple-sanctioned wakeup that does not route through APNS.

### Hard constraints this imposes
- **Wi-Fi-network-bound.** The push provider only runs while the device is
  joined to a designated Wi-Fi SSID (`matchSSIDs`, a list, so multi-SSID
  sites are fine; the app takes them comma-separated since 2026-09-13).
  A handoff between two listed SSIDs is a network change: the provider
  stops (`NEProviderStopReason` 3, no network) and restarts on the new
  one, and the app registers again from its new address. A call placed
  in that window is dialled to the registration the server holds at the
  moment, which may be the old address — so the server watches the
  registry while a callee's INVITE is in flight and re-dials the new
  route the instant a registration from elsewhere arrives
  (`registry.WaitRouteChange`, `retargetError` in `internal/b2bua`, at
  most 3 re-targets per call); before that, every such call ran into
  Timer B (32 s) and died (2026-09-13 two-SSID test). An INVITE on a
  route that has given no response is abandoned at once when the caller
  hangs up, the callee moves or the wake is declined (sipgo's forced
  cancel; with a response, the normal CANCEL/487 runs) — sipgo otherwise
  waits for Timer B before it will cancel, and the wake_cancel came 20–30
  s after the caller had gone, so the app rang for nobody. The wake sent
  on the old, dead extension connection is replayed to whichever session
  of the device connects next within the ring window; the client itself
  reconnects the moment Network.framework reports a better path for its
  connection (`betterPathUpdateHandler` in `LANSocketTransport`, Apple's
  own migration guidance), which is what a handoff to a new address
  produces. The SIP stack must learn the new address too: baresip lists
  its interface addresses once at start (the netroam module, not loaded
  here, is what would refresh them), so the shim re-reads them before
  every transport reset — with the stale list the rebuilt transport and
  every call's media socket bound the old address and the app answered
  each INVITE with 500 Call Error (2026-09-13, the second SSID test:
  "unavailable" on the caller's phone, a few seconds of CallKit on the
  iPhone). Off that network there are no wakeups and the phone is
  unreachable for incoming calls: this is the accepted product scope, not a
  bug. It matches the on-prem goal ("works even if internet is down, as long
  as local Wi-Fi is up"). Remote reach (cellular / home Wi-Fi) is unscheduled
  — see §6 "Much later". LPC is the **only** wake path; there is no APNS
  fallback (§9 risk 1).
- **App Store distribution.** SSID configuration is done in-app through
  `NEAppPushManager` (no MDM). Requires the Network Extension entitlement
  (`com.apple.developer.networking.networkextension` → `app-push-provider`).
  App Review will scrutinize Network Extension usage; justify the on-prem use
  case clearly.
- **Extension is resource-limited.** Keep the extension to signaling only — no
  media. Media runs in the main app once foregrounded by CallKit.
- **The extension must reconnect on its own, fast, for ever.** iOS only
  restarts the provider on a network change and calls its timer rarely, so
  a server restart or any dropped connection would otherwise leave the
  device unreachable until Wi-Fi is toggled (seen 2026-09-11). The provider
  runs the same `GatewaySession` keeper as the app, permanently active:
  backoff 1, 2, 3, then 5 s for ever, and an attempt parked in
  Network.framework's "waiting" (server not yet listening) is abandoned
  after 3 s. The system timer only kicks the keeper. A fatal gateway error
  (rejected enrolment) stops the fast retry; the timer retries those.

### LPC is only the background-survival path
This is the single most important framing for testability. The wake transport is
**not** special when the app is running:

- **App alive (foreground):** the app holds a **direct LAN socket** (WebSocket /
  TLS) to the on-prem server. No LPC, no APNS involved. This is real production
  behaviour, not a test stand-in.
- **App dead / backgrounded:** the `NEAppPushProvider` extension holds that
  *same* socket and reports incoming calls to CallKit.

Both speak the **same wire protocol to the same server**, and the app and
extension **share the protocol code** (App Group framework). Therefore LPC is a
thin background-delivery sibling of the foreground LAN socket — which is what
lets ~everything be validated with no device (see §7).

## 3. Decisions (locked)

| Area | Decision |
|---|---|
| SIP + media stack | **baresip / libre / librem** (BSD-3), **Opus** (BSD) app↔app; **G.722** (public-domain implementation, WebRTC's copy — §8) and **G.711** on PBX calls — fully permissive, commercializable, no license fee |
| PBX target | **Asterisk** for dev/test; compatible with **Cisco CUCM** and general SIP exchanges → server is a standard SIP element, PBX-agnostic. **PBX is optional** — the server also routes app↔app calls directly with no PBX present |
| Exchange model | **Own exchange.** The light server is the call controller. Any PBX is a SIP trunk peer only. The app never registers to a PBX, in dev or prod (§4.4) |
| App↔server leg | **SIP (baresip) under a strict private profile** (§4.4). This is the long-term design (§4.5); the `CallEngine` seam remains as ordinary structure, no replacement is scheduled |
| Wire framing | **Length-prefixed JSON over TLS 1.3** (port 7443) for every signal transport: foreground LAN socket, LPC extension socket, public edge. No WebSocket — raw TLS is the natural `NWConnection` fit for the extension. Frozen in [protocol/PROTOCOL.md](protocol/PROTOCOL.md) with golden fixtures shared by Go and Swift |
| Server language | **Go**, standard library only (no external modules) |
| Coverage | **Wi-Fi-only (LPC).** Remote app users (cellular / home Wi-Fi) are **unscheduled** (§6 "Much later"). The wake transport stays abstracted so an APNS sibling could be added without a rework, but none is planned |
| Distribution | **Public App Store**, in-app LPC configuration |
| Video / IM | **Not scheduled** (§6 "Much later") |

## 4. Architecture

```
┌──────────────────────── iOS device (on-prem Wi-Fi) ─────────────────────────┐
│                                                                              │
│  Main App (SwiftUI)               App Push Provider Extension (NE target)     │
│  ├─ CallEngine  (SIP/RTP)         ├─ PushTransport (persistent TLS → server)  │
│  ├─ CallKit control               ├─ reports incoming call → CallKit          │
│  ├─ AddressBookStore              └─ foregrounds main app on answer           │
│  ├─ DirectorySync                                                             │
│  └─ AudioSession (Opus/G.722/G.711) Shared framework (App Group):            │
│                                    protocol models, keychain, config          │
└─────────────────────────────────────┬────────────────────────────────────────┘
                                       │ TLS, transport-agnostic wake/signal
                                       ▼
                    ┌─────────────────────────────────────┐
                    │  Light Server (on-prem, Go or Rust)  │
                    │  ├─ Wake gateway (LPC conns)         │
                    │  ├─ SIP B2BUA / registrar  ◄── core  │
                    │  ├─ Media relay (app-leg RTP)        │
                    │  ├─ local routing (app↔app, no PBX)  │
                    │  ├─ PBX event adapter (AMI/ESL) ◄─── │
                    │  │   OPTIONAL, dev-only              │
                    │  └─ Directory API (address book)     │
                    └──────────────┬──────────────────────┘
                                   │ standard SIP (trunk/registration)
                                   ▼   — OPTIONAL when calling app↔app —
                  Existing PBX — Asterisk (dev) / Cisco CUCM / general
```

### Standalone mode: app-to-app calls with no PBX
The light server is a full SIP element in its own right, so it can route calls
**directly between two registered app instances without any PBX**:

- Each app registers its SIP account to the light server, which keeps an
  internal registration/location table of online endpoints (and the
  wake-routable identity for offline ones).
- An outbound call to another known user is routed by the server itself: if the
  callee's app is online it forwards the INVITE; if offline it fires a **wake**
  (LPC), waits for the app to register, then bridges the legs as a B2BUA — the
  same wake path used for PBX-originated calls.
- The PBX is therefore **optional**. With no PBX configured, the server is a
  self-contained on-prem call controller for the app fleet. With a PBX
  configured, the server additionally trunks/registers to it for external reach.

Routing decision (server): destination resolves to a **local endpoint** →
route internally; otherwise → hand off to the configured `PBXAdapter` trunk
(if any). Address-book entries carry which mode applies, so the app UI is
identical for both. This reuses the wake + B2BUA machinery — app↔app is not a
separate code path, just a routing target with no PBX leg.

### Why the server is a SIP B2BUA, not an AMI/ESL listener
A killed/backgrounded app cannot hold a SIP registration, so the PBX would have
nowhere to send the INVITE. The light server solves this **and** stays
PBX-agnostic by being the SIP endpoint the app registers to:

1. The app's SIP account points at the **light server** (acting as outbound
   proxy / registrar / B2BUA).
2. The light server maintains a registration/trunk toward the real PBX
   (Asterisk, CUCM, …) so the PBX always considers the line reachable.
3. Inbound INVITE arrives at the light server → it sends a **wake** over LPC →
   the extension reports to CallKit → on answer the app registers SIP → the
   server bridges the legs (B2BUA).

This depends only on standard SIP, so it ports from Asterisk to CUCM and other
exchanges. AMI (Asterisk) / ESL (FreeSWITCH) are kept only as **optional
dev/test adapters**, never as the backbone. (CUCM exposes no such API, so the
core flow must not need one. Note CUCM requires third-party SIP devices to be
provisioned appropriately — this is a server↔PBX integration concern, isolated
from the app.)

### 4.4 App-leg profile (locked)
The app↔server leg is SIP, but **not generic SIP**. It is a private profile
between two components we own. These are MUST rules from Phase 0; each is cheap
now and expensive to retrofit once a B2BUA has shipped with UDP or direct-media
assumptions baked in. Several were originally justified by remote users (§6
"Much later"); they stand on their own for a shared office Wi-Fi, where TLS-only
signalling, relayed media and SRTP are the right answer regardless, and they
are already implemented — do not remove them because remote reach is deferred.

1. The app registers **only to the light server**. Never to a PBX — including
   Asterisk in dev.
2. App-leg SIP is **TLS over TCP only**. No UDP listener exists on the
   app-facing side. Every REGISTER and initial INVITE on it is authenticated
   with **SIP Digest using the device's enrolment credential** (username =
   device id, password = token; the server stores only H(A1)), and the SIP
   user must be the one the device is enrolled for. In-dialog requests and
   the trunk (rule 7) are not challenged. The Digest realm is the SIP
   domain (`-local-domain`), so renaming the domain invalidates every
   stored credential until devices are re-enrolled; treat the domain as
   permanent (README, "The SIP domain").
3. App-leg media **always relays through the server**. ICE/TURN may be added
   later as an optimisation; the relay path is the baseline and must exist from
   Phase 0.
4. **SRTP mandatory.** Codecs: Opus primary app↔app; G.722 primary and
   G.711 fallback when a leg is the PBX trunk. Both legs of a call always
   share one codec — the relay copies encoded audio and never transcodes;
   a transfer whose new far leg takes another codec moves the remaining
   leg to it by re-INVITE.
5. Wake, directory and presence travel on the **wire-protocol channel**.
   SIP carries call setup and media only.
5a. **QoS on every hop** (Phase E, 2026-09-13): media is marked DSCP EF
   (46) and signalling CS3 (24) by the app, the server and the PBX. App:
   baresip `rtp_tos 184` / `sip_tos 96`, and a libre patch that also sets
   Apple's `SO_NET_SERVICE_TYPE` (voice / signalling — what iOS maps to
   the Wi-Fi WMM access category; the DSCP byte alone does not) and
   `IPV6_TCLASS`; the wire-protocol socket uses `NWParameters.serviceClass
   = .signaling`. Server: RTP/RTCP sockets EF and the SIP and wire-protocol
   listeners CS3 through `net.ListenConfig` Control hooks (`internal/qos`,
   vendored `media.ListenConfig` / `sipgo.ListenConfig`), accepted TLS
   connections inheriting the listener's mark. PBX: `tos_audio=ef`,
   `cos_audio=5` on endpoints and `tos=cs3`, `cos=3` on transports.
   Gate: `make harness-qos` captures the server's egress and requires
   every RTP packet EF and every SIP/TLS packet CS3.
6. **REFER (transfer) is handled by the server** as B2BUA, never proxied
   through to the far leg. When the remaining party and the target are both
   on the PBX, the server re-issues the transfer to the PBX (its own REFER
   on the trunk leg) so the PBX completes it and this server leaves the
   media path; if the PBX refuses, the server completes the transfer
   itself. The app's leg is unaffected either way.
7. The `PBXAdapter` speaks **trunk SIP and nothing else**. AMI/ESL remain
   dev-only.

### 4.5 Why SIP on the app leg
SIP is kept on the app leg because baresip supplies SDP negotiation, codecs,
SRTP, jitter buffer, hold, and transfer for free; because the headless
baresip ↔ server ↔ baresip harness (§7.2) depends on both ends speaking SIP;
and because the B2BUA stays symmetric — one SIP stack for both legs.

This is the **long-term design**. The two events that would once have
prompted a switch to WebRTC — video (libwebrtc arriving anyway) and remote
users' Wi-Fi → cellular handoff quality — are both unscheduled (§6 "Much
later"), so no replacement is planned. The app still sees the leg only through
the `CallEngine` protocol and the server terminates it in the B2BUA; that seam
stays as ordinary good structure, not as a migration path.

Consequence: baresip plus the vendored `audiounit` driver patches
(`ios/vendor/patches/`, full-file replacements that implement the CallKit
manual-audio contract) are permanent, maintained code. Those patches are
fragile against upstream baresip changes, so a baresip upgrade is a deliberate
task (re-apply, rebuild, re-run `make audio-probe-sim` and `make sim-call`),
never a routine bump.

### 4.6 Remote users (off-prem app users)
Deferred, unscheduled — see §6 "Much later", which keeps the design notes. The
per-device enrolment credential that section calls for already exists for the
wire-protocol gateway (`server/internal/enroll`).

## 5. Test-first component contracts

Everything sits behind a protocol so the app logic is testable without real SIP,
real LPC, or a real PBX. Built and verified in dependency order:

| # | Component | Interface (Swift) / artifact | How it's tested independently |
|---|---|---|---|
| 1 | **Wire protocol** | versioned message schema (length-prefixed JSON or protobuf) | round-trip / golden-file tests, no platform dep |
| 2 | **Wake gateway** (server) | connection + auth + fan-out | mock client + mock event source, unit + integration |
| 3 | **SIP B2BUA/registrar** (server) | SIP in/out | **dockerized Asterisk** + **SIPp** scenarios; profile conformance: SIPp asserts UDP and plain-TCP are refused and direct-media SDP is rewritten to the relay (§4.4) |
| 3a | **Media relay** (server) | RTP/SRTP relay between legs | headless baresip pair; assert no direct media path exists |
| 4 | **Directory API** (server) | REST + sync semantics | HTTP-level tests |
| 5 | **`PushTransport`** (Swift) | `connect / receiveWake / send` | in-process fake server implementing the frozen protocol |
| 6 | **`CallEngine`** (Swift) | `register / dial / answer / hold / transfer / events` | mock for unit tests; dockerized Asterisk for integration |
| 7 | **CallKit layer** | thin adapter over CallEngine/PushTransport | driven by the two mocks |
| 8 | **`AddressBookStore` + `DirectorySync`** | local store + reconcile | persistence + sync-conflict tests |
| 9 | **`AudioSession` / media** | Opus/G.722/G.711 capture/playback | integration / manual call tests |
| 10 | **Device enrolment / auth** (server + Swift) | issue / verify / revoke device credential | HTTP-level + keychain tests |

Key abstractions to keep future-proofing cheap (see §7 for how each is tested
without a device):
- `SignalTransport` protocol with two impls: `LANSocketTransport` (foreground
  LAN socket + automated-test sibling) and `LPCTransport` (`NEAppPushProvider`,
  background survival). The app/server treat a delivered message identically
  regardless of impl; an `APNSTransport` sibling is the much-later addition the
  seam permits (§6).
- `CallEngine` protocol isolates `libbaresip` so the stack is swappable and
  mockable.
- `AudioIO` seam: the iOS CoreAudio/`AVAudioSession` backend has a non-Apple
  sibling (baresip `aufile`/portaudio) used for automated WAV-based media tests.
- `PBXAdapter` on the server isolates Asterisk-specific bits from the SIP core.

## 6. Build phases

Everything is built and validated on `LANSocketTransport` first (which is also
the real foreground transport), then LPC is verified in isolation and switched
on by config — see §7.4.

- **Phase 0** — Freeze wire protocol; Go server skeleton (signal gateway + SIP
  B2BUA/registrar + routing + media relay); docker harness (Asterisk + SIPp +
  headless baresip). All headless. **B2BUA engine spike: DECIDED — sipgo +
  diago** (both BSD-2 / MPL-2.0, commercializable; builds with `CGO_ENABLED=0`).
  diago provides dialog control, a media-forking bridge (the app-leg relay),
  and REFER; the registrar stays ours because diago handles INVITE but not
  REGISTER. **Proven headless** (`make spike-call`): baresip 201 → B2BUA →
  baresip 202 over TLS 1.3, Opus, media bridged through the server and
  asserted on the callee's recording. Two lessons baked in: diago must set
  `TLSURINoSIPS` (libre cannot route a `sips:` Contact), and the harness
  builds baresip 3.x from source (Debian's 1.x has no WAV player). CUCM
  third-party registration is deferred to the PBX phase. The hand-written
  `internal/sip` + `internal/relay` become the fallback/model, not the call
  path.
- **Phase 1** — **App↔app call end-to-end over `LANSocketTransport`** (no PBX):
  wake → CallKit → answer → audio. Validated in simulator + headless baresip.
  *Server half done 2026-09-05:* `internal/b2bua` (sipgo + diago) is the
  app-leg element; `make harness-call` proves a bridged 201→202 call with
  media through the server; unregistered callee → gateway wake →
  `registry.WaitRegistered` → bridge, or fast 480 when no connection can be
  woken. `make harness-wake` proves the wake path headless: callee with no
  UA → wake over the gateway → fake app registers it (~50 ms) → bridged →
  audio asserted. *iOS signalling + CallKit cut done 2026-09-05:*
  `ios/DiallerCore` (transport, session machine, call controller; 12 tests)
  and `ios/Dialler` (app + `NEAppPushProvider` extension) type-check against
  the iOS SDK. *2026-09-06:* rings on a real iPhone; Local Push
  configuration saves (a stale NE config had to be removed first);
  `ios/vendor/build-baresip.sh` cross-compiles libre/baresip 3.15 + Opus +
  OpenSSL to XCFrameworks and `ios/DiallerEngine` wraps them as the
  `CallEngine`, building for device and simulator. Remaining: the first real
  device call with audio, and the killed-app wake through the extension.
- **Phase 2** — Outbound + hold + transfer; PBX leg via Asterisk.
  *2026-09-07:* Phase 1 closed on a real iPhone (first and subsequent calls,
  killed-app wake via the Local Push extension, lock screen); the root
  causes were the audio driver creating CoreAudio units before CallKit's
  activation (`!pri`), the B2BUA answering the caller before dialling the
  callee, and a registration-refresh race. Outbound calls done: keypad and
  directory share one dial path (`sip:<target>@<domain>`), CallKit start
  action, in-call screen with mute/speaker/end. PBX leg done: `-trunk`
  makes the server a SIP trunk peer (UDP/TCP/TLS), non-local destinations
  route to it, G.722 (or G.711) on both legs of a trunk call (no transcoding), both
  directions proven headless by `make harness-trunk`. Headless loop for the
  app path: `make sim-call` runs the real engine on the iOS simulator
  against the harness and asserts RTP + rendered audio energy. Hold/resume
  done (re-INVITE through the bridge; media stops and restarts, asserted
  headless). Blind transfer done: REFER is consumed by the server, the
  target routed like a fresh call (local, wake, trunk, echo), the remaining
  party re-bridged, the transferor released with a final NOTIFY; asserted
  headless. *2026-09-09:* a transfer whose remaining party and target are
  both on the PBX is handed to the PBX (REFER offload on the trunk leg,
  server-side fallback if refused), so the server drops out of the media
  path instead of hairpinning the PBX; `make harness-trunk` asserts it.
  Echo self-test destinations: `echo` (server) and `600` (PBX).
  Remaining in Phase 2: attended transfer; ring-back or hold music for the
  party waiting during a transfer (codec renegotiation on transfer to the
  trunk done 2026-09-12, near-term item 2).
- **Phase 3** — Address book store + server-driven directory sync. *Built:*
  delta sync with tombstones (`DirectoryClient` / `AddressBook`), server-side
  de-duplication by URI (a re-seeded directory collapses duplicates), and
  incoming-caller naming from the directory (directory name → caller's own
  display name → bare number; a synchronous in-memory lookup, no call delay).
- **Phase 4** — `LPCTransport`: device-verify against the transport conformance
  suite, then flip background wakes to LPC. *Proven on device 2026-09-07:*
  killed-app wake through the `NEAppPushProvider` extension and answer from
  the lock screen. *2026-09-10, session resilience:* iOS kills the app's
  sockets while it is suspended; the app used to stay "disconnected
  (ECONNABORTED)" and unable to dial until relaunched. `GatewaySession`
  (DiallerCore) now reconnects with backoff while the app is active,
  reconnects at once on return to the foreground, abandons an attempt stuck
  in Network.framework's "waiting", and the engine rebuilds its SIP
  registration on the new welcome (retrying failed registrations itself).
  The SIP stack also used to die after such a restart: libre binds its
  loop context to the thread that initialises it, and the shim initialised
  on the caller's thread, which after a reconnect is a GCD worker that
  libdispatch retires when idle. The shim now owns the whole stack on its
  loop thread (see `ios/vendor/patches/README.md`), with a watchdog and
  stack rebuild as the backstop. The server forgets a pending wake once its
  call has been answered and ended, so a reconnect is never rung for a
  finished call (it was, within the 30 s expiry). The client treats a
  gateway drop as no call event at all: a ringing wake may have come via
  the extension and its INVITE arrives through SIP, so ending wake-only
  calls on a drop broke the background wake (caught by
  `testSessionDropDoesNotEndAnExtensionDeliveredWake`).
  *2026-09-11, "ua_alloc -100" after every unlock:* libre caches SIP TLS
  connections and sends on a cached one synchronously; after a suspension
  that connection is dead and the write fails with EPROTO, and nothing
  evicts it. The engine now resets the SIP transports (baresip's
  network-change reset) whenever it has reason to doubt its flow: after a
  gateway drop, before a doubted re-REGISTER, and before each retry.
  Headless: `RESTART_SERVER=1 make sim-call` (in `sim-call-all`).
- **Remaining near-term (on-prem softphone completion)**, in rough priority:
  1. **App-leg authentication.** *Done 2026-09-10:* SIP Digest on REGISTER
     and initial INVITE (`internal/sipauth`), username = device id, password
     = enrolment token, realm = the SIP domain; the server keeps only H(A1)
     (`enroll.Store.DigestSecret`) and refuses a SIP user other than the
     device's enrolled one, so an enrolled phone cannot register or call as
     another. Trunk and in-dialog requests are not challenged. The
     directory/admin API moved to TLS on the same certificate (the device
     token no longer crosses the LAN in clear). Harness phones, SIPp, the
     wake test and the simulator loop carry the fixed dev credentials, and
     `make harness-test` includes a refused unenrolled registration.
  2. *Done 2026-09-12:* codec renegotiation when an Opus app call is
     transferred to the trunk — the remaining app leg is moved to the codec
     the trunk took (G.722) by re-INVITE (`renegotiateCodec` in
     `internal/b2bua/transfer.go`; the vendored diago `ReInvite` now applies
     the answer's SDP), 488 only if the remaining party refuses; proven by
     `DIRECTION=xfer-app make harness-trunk`. Still open: attended transfer;
     ring-back or hold music for the party waiting during a transfer.
  3. Real PBX interop beyond Asterisk (CUCM third-party SIP device
     provisioning, §9 risk 4).
  4. Before release: third-party acknowledgements screen and the App Store
     export-compliance declaration (see `ios/README.md`).
  5. Mouth-to-ear latency measurement on the echo path and jitter-buffer
     tuning (parked 2026-09-07). *2026-09-10:* the "received audio 0–2 s
     late, varying per call" symptom was the media relay, not the app: the
     library's RTP writer paces one packet per 20 ms, so a relay built on
     it could never drain a backlog and every burst (call-start ordering, a
     Wi-Fi stall) became permanent delay. The relay now forwards each
     packet at once carrying the source's own timestamps (`pump.forward`),
     and its 2 s log line reports arrival skew (`early_max_ms` /
     `late_max_ms`) so the delay is visible numerically. Unit tests drive a
     50-packet burst through the real reader/writer in <1 ms (was 1.0 s).
     *2026-09-12 (audio-quality plan, phases B and A done):* measured
     first. The harness now reports mouth-to-ear delay and gaps
     (`assert_audio.py --reference`, §7.2) and can impair the server's
     egress (2 % loss, 30 ± 10 ms jitter). Baseline in docker: 120 ms on
     both echo paths, no gaps; impaired: 160 ms and one audible gap per
     lost packet. Three defects behind that, all fixed: baresip 3.15 never
     called the codec's concealment on receive (vendored patch in
     `ios/vendor/patches/apply-baresip.sh`, applied in the harness phone
     too); the server's SDP told every phone `useinbandfec=0` (vendored
     diago patch, `sdp.OpusFmtp`: FEC on, mono, 32 kbit/s); and baresip's
     Opus module only produces or decodes FEC with `opus_packet_loss` set.
     App profile (`BaresipCallEngine.stackConfig`, pinned by
     `StackConfigTests`, mirrored in `harness/baresip/config`): adaptive
     jitter buffer 40–160 ms, play buffer 40–160 ms adaptive, Opus voip /
     FEC / 32 kbit/s mono / complexity 6, RTP stats on, dead-media timeout
     30 s, EF marking. Result on the impaired server echo: 120–130 ms,
     single losses recovered from FEC, at most one 20–40 ms gap per call
     from a double loss (the jitter buffer withholds the next packet after
     a loss until it refills; concealing at the missed slot rather than on
     the next arrival would close it — plan Phase D). Two gotchas worth
     keeping: mono Opus is listed as `opus/48000/1` in the account
     (baresip names it by audio channels; `/2` silently falls back to
     PCMU), and Opus stereo at this bitrate is CELT mode, which has no FEC.
     *2026-09-12 (Phase C done):* G.722 wideband end to end on PBX calls,
     never transcoded. The trunk is offered `g722,pcmu,pcma` (server flag
     `-trunk-codecs`; `pcmu,pcma` for a narrowband-only PBX) and the app
     leg Opus, G.722, G.711, so a call that touches the trunk lands on
     G.722 on both legs and app↔app stays Opus. The app's `g722` module is
     built from WebRTC's public-domain G.722 (baresip's own needs spandsp,
     LGPL — `ios/vendor/patches/g722`, provenance in §8); the vendored
     server SDP knows static PT 9; Asterisk allows g722 first. Asserted by
     `make harness-trunk` (`codec=G722` on both legs, both directions, gap-
     free recordings) and the app↔app→trunk transfer above. Fallback is
     proven too: `NARROWBAND=1 make harness-trunk` builds the harness PBX
     with ulaw/alaw only and every scenario must land on PCMU. That test
     exists because the first device run failed: the vendored diago
     narrowed a bridged callee's offer to the caller's *first* common codec
     (fine while that was always PCMU), so a G.711-only PBX was offered
     G.722 alone and the INVITE failed — the offer now carries every common
     codec, in the caller's order (`server/vendor/PATCHES.md`).
     *2026-09-12 (Phase D done):* honest loss and concealment. The relay
     now rebases the source's RTP sequence numbers onto its own instead of
     renumbering every packet (vendored `WriteSamplesSeq`), so a packet
     lost upstream stays missing on the way out and the far end's jitter
     buffer and concealment see it; before, a loss became a silent hole in
     a seamlessly numbered stream that no decoder could conceal. G.711 and
     G.722 got packet-loss concealment in the app — baresip only had it
     for Opus — as our own unit (`ios/vendor/patches/plc`: pitch-period
     repetition with overlap-add and a fade, after ITU-T G.711 Appendix I,
     no third-party code; `make plc-test`), compiled into both modules and
     into the harness phone. Measured on the impaired PBX loop (2 % loss,
     30 ± 10 ms jitter, 12–18 drops): 7 gaps of 40 ms before; after, one
     or two dips of 20–40 ms per call (three runs: 1×20, 1×20, 2×40 ms);
     the gate is now ≤ 2 gaps ≤ 40 ms on both loops. The residual is
     baresip's jitter buffer refilling after a loss (`jbuf_get` withholds
     while `nf <= wish`; concealment runs only when the next packet is
     pulled, so the player underruns for a frame first). Counting the
     missing packets as frames so the next one is released on time was
     tried and reverted the same day: it drained the buffer and made
     things worse (4–13 gaps). Closing the last 20–40 ms needs concealment
     driven from the playout timeline (decode on a timer when a slot's
     packet has not arrived), a receive-path redesign — left open.
     Escalation if concealment on G.722/G.711 cannot meet the loss target
     on the real WLAN: an app-leg Opus transcoder in the server, kept out
     on purpose (latency, tandem coding, breaks the copy-relay and the
     std-lib-only server).

### Much later (not scheduled)

Kept here so the design intent is not lost and so existing references to the
old phase labels still resolve. None of this is on the roadmap.

- **Remote users (was Phase 4b).** Staff away from the site, on cellular or
  home Wi-Fi. Each item maps to an existing seam; none changes the call logic:
  - *Wake:* `APNSTransport` (PushKit VoIP push) via the `SignalTransport`
    seam. Must report to CallKit immediately on receipt or iOS penalises the
    app. Same wire-protocol payload as LPC.
  - *Public edge:* a SIP/TLS listener + media relay exposed to the internet —
    a session-border-controller role in the same binary. The PBX stays behind
    it and is never exposed. Needs rate limiting, auth-before-INVITE, no UDP.
  - *Device auth:* the per-device enrolment credential already used by the
    gateway, extended to the SIP leg (which near-term item 1 delivers anyway).
  - *Reconnect / resume:* a `resume_token` in `welcome` (PROTOCOL.md §7, v1.1
    additive) so a device leaving the SSID re-attaches over cellular; SIP
    re-registers and re-INVITEs for in-progress calls.
  - *Known weakness to measure first:* mid-call Wi-Fi → cellular handoff on
    the SIP leg is re-register + re-INVITE, clunkier than a WebRTC ICE
    restart, and drops a second or two of audio. If unacceptable, that is the
    one reason to revisit §4.5.
  - The APNS sibling would also have to pass the §7.1 transport conformance
    suite on-device.
- **Video (was Phase 5).** WebRTC/BSD + VideoToolbox. If it ever happens,
  libwebrtc could carry audio too and retire baresip; that is the second
  reason to revisit §4.5.
- **IM (was Phase 6).** SIP MESSAGE/SIMPLE or XMPP, riding the wire-protocol
  channel like presence does.

## 7. Validation strategy: maximise automation, bound the human touch

Principle: **every Apple-hardware dependency is hidden behind a seam that has a
non-Apple sibling**, and both siblings pass the *same* conformance suite. The
non-Apple sibling runs in CI / headless; the Apple sibling runs the identical
suite once on-device. Switching siblings changes delivery, not behaviour.

### 7.1 The three seams
| Seam | Automated sibling (no human) | Apple sibling (device) | Conformance suite proves |
|---|---|---|---|
| Signal transport | `LANSocketTransport` (LAN) | `LPCTransport` (`NEAppPushProvider`) | message semantics, ordering, reconnect, auth |
| Audio I/O | baresip `aufile`/portaudio (WAV in/out) | CoreAudio + `AVAudioSession` | SIP + Opus + jitter buffer (assert on recorded WAV) |
| Call UI | headless call-state observer | CallKit (`CXProvider`) | ring/answer/hold/transfer/end transitions |

### 7.2 Fully automated, zero human (most of the system)
- Wire protocol round-trip + golden fixtures.
- Go server: signal gateway, app↔app + PBX B2BUA routing, directory sync.
- **Real SIP + real media, headless**: `baresip-A` ↔ Go server ↔ `baresip-B`
  (+ Asterisk for the PBX leg) in docker-compose; place calls, hold/transfer,
  and validate RTP/Opus by playing a known WAV from A and asserting on B's
  recording. The iOS app wraps the *same* `libbaresip`; only the audio backend
  differs (seam #2). *Audio-quality gates (2026-09-12):* the WAV is an
  aperiodic tone pattern, so a recording can be correlated against it —
  `harness/spike/assert_audio.py --reference` reports the mouth-to-ear delay
  of the path and every gap inside a tone burst. `make harness-echo` bounds
  both (≤ 150 ms on the server echo, ≤ 200 ms through the PBX, no gaps);
  `IMPAIR=1` adds 2 % loss and 30 ± 10 ms jitter to everything the server
  sends (`harness/netem`) and judges what the phone's decoder conceals. The
  relay logs `lost`/`reordered`/`jitter_ms` per leg from the sequence numbers
  and timing it sees, and `sim-call` fails on any loss in a clean run.
- `LANSocketTransport` end-to-end: server wake → transport → the exact app code
  path that handles an incoming call.
- Swift logic modules via `swift test` (CI; or locally if the sandbox grant is
  added — optional, not on the critical path).
- Simulator: full app + CallKit control flow over `LANSocketTransport`, via
  XCTest in CI.

### 7.3 Irreducible human-touch list (the entire manual surface)
1. Extension launches on matched-SSID join and survives app kill / background.
2. `reportIncomingCall` from the extension shows CallKit and cold-launches the
   app on answer.
3. Real bidirectional audio + route changes (earpiece/speaker/Bluetooth/CarPlay).
4. `LPCTransport` passes the transport conformance suite on-device.

Each is a short checklist backed by structured os_log/signpost output — not an
open-ended "test the app".

### 7.4 Rollout: confirm on WebSocket, then switch to LPC
1. Build and fully validate everything on `LANSocketTransport` (headless + CI +
   simulator). Not throwaway — it is the production foreground transport.
2. Build `LPCTransport` and device-verify it in isolation against the transport
   conformance suite + items 7.3.1–2. No call-logic changes are in scope.
3. Flip config so background wakes use LPC; re-run the 7.3 device smoke. Identical
   behaviour by construction.

## 8. Licensing (all commercializable)

| Component | License | Notes |
|---|---|---|
| baresip / libre 3.15 | BSD-3 | SIP + RTP, modular C, production-grade (verified from the licence files in `ios/vendor/src`) |
| Opus 1.5 | BSD-3 | primary audio codec; royalty-free IPR declarations at the IETF |
| OpenSSL 3.3 | Apache-2.0 | TLS + SRTP crypto; the largest licence surface in the app (3.x only — 1.x carried the old OpenSSL/SSLeay licence) |
| G.722 (WebRTC's copy of Steve Underwood's implementation) | public domain; WebRTC's edits BSD-3 | wideband codec on PBX calls, compiled into the app's `g722` module instead of spandsp (LGPL, which baresip's upstream module needs). Source revision, licence check and SHA-256 recorded in `ios/vendor/patches/README.md` |
| G.711 | royalty-free (baresip's own module) | narrowband fallback |
| Packet-loss concealment (G.711/G.722) | own code, `ios/vendor/patches/plc` | written from the ITU-T G.711 Appendix I description; no spandsp or other third-party PLC |
| G.729 | patents expired (~2017) | optional |
| sipgo / diago (server) | BSD-2 / MPL-2.0 | **decided** in Phase 0; vendored, builds with `CGO_ENABLED=0` |
| Server runtime (Go) | permissive stdlib + vendored deps | — |
| WebRTC / libwebrtc | BSD-3 | video, **much later** (§6) |

All permissive and fine to link statically into a closed-source App Store app.
Both BSD and Apache-2.0 still require the copyright notices, licence texts and
warranty disclaimers to be reproduced in a binary distribution: an
acknowledgements screen (or bundled licence file) is pending — see
`ios/README.md`, which also covers the export-compliance declaration.

Explicitly avoided: PJSIP / Linphone (GPL-or-paid; GPL conflicts with closed
App Store distribution). Kamailio, rtpengine, and Asterisk as *embedded*
engines (GPL) — Asterisk stays as the dev/test PBX peer, which is fine because
it is neither linked nor redistributed.

## 9. Top risks to validate early

1. **LPC entitlement + App Review — now the single, unmitigated dependency.**
   LPC is the only wake path (APNS is unscheduled, §6 "Much later"), so an
   ungranted or rejected `app-push-provider` entitlement has no fallback.
   Validate earliest: request the entitlement and put an external TestFlight
   build through review before investing further in the on-device flow.
2. **Unauthenticated app leg** — closed 2026-09-10 (§6 near-term item 1:
   Digest bound to the enrolled device on REGISTER and INVITE; directory
   API on TLS). Residual: a nonce may be reused within its 5-minute window
   (no nonce-count tracking), acceptable on a LAN app leg over TLS.
3. **Extension longevity/resource limits** holding a persistent connection.
4. **CUCM SIP interop** quirks vs. Asterisk (third-party SIP device
   provisioning, registration behaviour) — isolate behind `PBXAdapter`. Now on
   the core path: the PBX leg is the product, not an edge case.
5. **baresip ↔ CallKit ↔ AVAudioSession** lifecycle on answer (cold launch from
   extension) — resolved on device 2026-09-07 via the vendored audiounit
   patches (§4.5); the residual risk is keeping those patches working across
   baresip upgrades.
6. **B2BUA completeness.** Attended transfer across two legs, re-INVITE glare,
   transfer-to-trunk codec renegotiation (§6 near-term item 2).
7. **OPEN, under observation — wakes to a suspended app killed by iOS
   (2026-09-11; fixed the same day, working on device since; keep open
   until it has held for a while).** Reports `Dialler-2026-09-11-161148/161222/172336.ips`: `SIGKILL`,
   FrontBoard `0xBAADCA11` from callservicesd, main thread idle. Device
   console: every push that *launched* the app was reported to CallKit
   within 250 ms; every push into an *existing* process (suspended or
   backgrounded) reached no handler at all, and callservicesd killed the
   app 7 s after granting its VoIP assertion. First fix (synchronous
   `reportNewIncomingCall` in the PushKit delegate, a real defect per
   Apple DTS FB16655952) was on the phone for the 17:23 kill and did not
   suffice. Actual cause: Local Push Connectivity delivers an incoming call
   to the app's `NEAppPushDelegate`
   (`appPushManager(_:didReceiveIncomingCallWithUserInfo:)`, main queue)
   whenever the app process exists, and only launches a dead app through
   PushKit. The app never set that delegate, so for a live process the
   call went nowhere. Fixed: `AppModel` keeps the loaded `NEAppPushManager`
   and attaches `LocalPushDelegate` at every launch and after every save;
   both paths end in `handleExtensionWake`, which reports synchronously.
   Verified on device the same evening: (a) app in foreground, call from
   101; (b) use the app, lock, wait a minute, call; (c) close the app,
   restart the server, call — all ring. If it resurfaces: Analytics report
   `Dialler-…ips` with 0xBAADCA11, then a device console capture; the
   signatures to look for are in the iOS app notes.
   *Recurred 2026-09-13 00:51:24 (server time), narrower shape:* the app
   had been backgrounded six seconds earlier (`app: will resign active`
   is its last line) and was still resident; the extension acknowledged
   the wake, and the suspended process was killed with 0xBAADCA11 (its
   MetricKit crash report, pid 644, arrived by the diagnostics pipeline)
   without any of our code running — no `wake via extension`, no PushKit
   line. The three wakes that followed each launched a fresh process,
   which reported to CallKit within 100 ms and rang. So the delegate fix
   holds for a launch and for a long-suspended app (last week's test),
   and fails for a just-suspended one. Also seen that minute: CallKit
   refusing a report with `incomingcall error 3` (filtered by Do Not
   Disturb / Focus) — a system decision, logged as a 486 by the app.
   *Closed the same night from the device console:* callservicesd
   resumed the process and delivered the payload, and inside the app the
   framework logged `NEAppPushManager app has not set the delegate to
   receive the incoming call payload`; 7 s later `Killing VoIP app …
   because it failed to post an incoming call in time`. The delegate had
   been set — on the manager object the app itself created and saved when
   Local Push was re-enabled at 00:50:23. Deliveries go to the delegate of
   the instance the framework LOADS from preferences; the app's own saved
   object is not it. Every later process loaded the manager at launch and
   rang. Fix: after every save, reload from preferences and attach the
   delegate to the loaded instances (all of them, kept alive), and
   re-attach on `willResignActive`, the moment a delivery to a suspended
   process starts to matter. The phone's clock ran ~33 s ahead of the Mac's.
   *Two more from the same night's uploads (01:31–01:32), both fixed:*
   (a) ringing after the caller hung up on a locked phone — the server's
   `wake_cancel` went out at 01:31:01 to the only live connection (the
   extension), and the app's own session came up at 01:31:06; a cancel is
   now remembered until the wake's expiry plus 30 s and replayed to any
   session of that device that connects later (gateway `cancelled`,
   PROTOCOL.md §7, `TestCancelIssuedBeforeTheAppConnectedIsReplayed`), and
   the app resolves a cancel through a merged wake id when the call rang
   from its INVITE first; (b) one SIP call reported to CallKit twice —
   the INVITE's report was queued for the main thread from the SIP loop
   when the wake arrived on the main thread, found no CallKit entry yet,
   and reported again, so the caller's hangup ended only one of the two
   and the other rang until the user ended it. `CallKitBridge` now tracks
   queued reports: a reaffirm during that window counts as satisfied, and
   a queued report finds the call already reported and steps aside.
   *05:52 the same morning, two more:* (c) with the app in the foreground,
   the wake (gateway thread) and the INVITE (SIP loop) landed in the same
   millisecond; the INVITE path checked for a ringing call and inserted
   its own entry in two separate critical sections, so both passed their
   check first and one call rang twice — the check-and-insert is now one
   locked step; (d) the extension had been disconnected from 02:00 to
   05:52 while iOS reported it active, so every call found "callee
   offline, wake undeliverable"; its own log was lost because the app's
   launch upload deleted the shared file while the extension held it open
   (FileLog now reopens after a drain), and the extension's system timer
   now rebuilds the session from scratch whenever it finds itself
   disconnected. The server had not restarted (same instance, same
   session id prefix): the Mac was unreachable for those hours and came
   back, and the extension did not reconnect in the minutes after. The
   keeper's only hole for that: a connect attempt that reports nothing
   (no connected/waiting/disconnected) armed no timer, so one such attempt
   left it down for good. Every attempt now has a 10 s deadline
   (`GatewaySession.attemptTimeout`), after which it is torn down and the
   backoff continues; `testASilentAttemptIsAbandonedAtTheDeadlineAndRetried`.
   *Ring-after-hangup, the lasting cause (2026-09-13 17:02, third
   sighting):* the app's keeper deliberately holds no gateway session in
   the background, but a call the extension wakes rings in the
   background — so the caller's hangup (a wake_cancel on the app's
   socket, or its replay on connect) had nowhere to arrive, and neither
   did the app's own decline ack. The extension received every cancel
   within 1.5 s; the app only learned of them when the user opened it.
   Fix: the app holds a session for exactly as long as a call is tracked
   (`holdSessionWhileCallTracked` on the wake, `callEnded` releases it
   when the last call is gone and the app is not on screen); CallKit
   keeps the process running for that span. No new channel, no timer.
   *Confirmed the same day, 06:21, when the extension's log survived:* it
   logged `gateway error idle_timeout fatal=true` and nothing more for
   twelve minutes, until iOS stopped it for lack of a network. The
   server's fatal error frame was handled by `SessionMachine` as
   "emit the error, close the transport" — without a `.disconnected`,
   and `didClose` is a no-op once the state is closed — so the keeper
   never learned the connection was gone and never reconnected. Every
   fatal error is now a drop (`.protocolError` then `.disconnected`),
   the keeper retries the transient ones and stops on the refusals
   (`unauthorized`, `unsupported_version`, `superseded`) until the next
   foreground; and the machine mirrors the server's liveness rule: no
   frame from the server within 3 × heartbeat (a healthy link carries a
   pong per ping) closes the connection so a link the network silently
   lost is not held until TCP gives up. `testFatalErrorWhileLiveIsADrop`,
   `testASilentServerIsDroppedAfterThreeHeartbeats`,
   `testAFatalIdleTimeoutIsRetried`, `testARefusalStopsTheBackoffUntilTheNextForeground`.
   Why the extension's pings stopped reaching the server for 75 s on a
   locked phone still on Wi-Fi is not known (the frame it then received
   proves the downlink worked); the reconnect now covers it either way.
   *Same afternoon, two SSIDs on the list (§2):* moving between them
   does not restart the provider (both match), the phone's address
   changes, and the extension's connection dies without either side's
   TCP noticing. The server kept sending wakes into the old session
   until its 75 s idle close, and the extension only reconnected when
   its own 75 s liveness rule fired — every call placed in that window
   was lost. (A server-side ping was tried and reverted: it cannot reach
   a dead connection.) The fix is on the client and needs no timer: an
   NWConnection reports when the path under it is gone (viability) and
   when a better path exists; a handoff to a new address is exactly a
   better path, and the transport closes on it so the keeper reconnects
   at once over the new network (PROTOCOL.md §5).
8. **OPEN — SIP loop thread spinning at 100 % CPU; root cause not found
   (2026-09-12; spin contained the same day).** Report
   `Dialler.cpu_resource_fatal-2026-09-12-141400.ips` (iOS 26.6.2, the
   Phase C build): iOS killed the app after 48 s at 99 % CPU while in the
   background ("Non-Frontmost App"). Heaviest stack: the engine's loop
   thread → libre `re_main` → a kernel syscall, in a loop; 2 of 16 samples
   inside malloc from the same loop (the warning formatter). Reading:
   `fd_poll` was failing with `EBADF` — libre's Darwin "workaround"
   (`if (EBADF == err) continue;`) then retries forever without running
   timers or sleeping. `kevent` returns EBADF only when the kqueue
   descriptor itself is invalid. Consequences seen the same day: the
   shim's "loop died → rebuild" recovery could not fire (`re_main` never
   returned), every engine call from the app waited its 10 s timeout, the
   UI froze ("app won't even start" on a relaunch that hit the same
   state), and the server saw the app's registration go stale and a
   phone → app INVITE die on it (17:14 log; the stale-route case is
   closed since 2026-09-13 by the mid-ring re-target, §2). *Contained:*
   `ios/vendor/patches/apply-re.sh` bounds the retry (8 × 10 ms, with a
   warning naming `kqfd`/`nfds`) and returns EBADF; the loop thread marks
   itself dead and the engine rebuilds the stack. *Unknown:* what
   invalidated the kqueue descriptor. Two hypotheses, distinguishable by
   the new warning's `kqfd` value: (a) `kqfd=-1` — libre's own context
   was torn down under the running loop (`poll_close` via
   `re_thread_close`/`libre_close` on the loop thread from inside a
   handler, or the tss destructor); look at every `libre_close`/`stack_close`
   call site in `cbaresip.c` and at anything calling libre from a thread
   other than the loop; (b) `kqfd>=0` — some other code closed a
   descriptor number it did not own (a double `close()` — CoreAudio /
   audiounit teardown, Network.framework, or a baresip socket closed
   twice after a transport reset) and the kqueue's number was hit; look
   for the previous owner of that number in the console around the
   warning. Timing clue: the report started 114 s after the device woke
   and while the app was backgrounded, i.e. right after the wake/transport
   reset path (`resetRegistration` → `cb_reset_transports` →
   `uag_reset_transp`) ran. *Next time:* collect (1) the `Dialler…ips`
   from Analytics Data, (2) the app log / console lines containing
   `fd_poll EBADF` (the `kqfd` value decides between (a) and (b)) and
   `re_main returned` from the shim, (3) the server log around the same
   minute (a stale registration dialled, `context canceled`), (4) whether
   the app had just come back from a lock/unlock or a call end. With a
   symbolicated stack (keep the build's dSYM this time — the 42689BEC…
   image in the report could not be symbolicated because Xcode had
   already rebuilt), the frame above `re_main` names the caller.
   *Second episode, 2026-09-13 00:35–00:40 (with the diagnostics
   pipeline in place):* two normal calls, `app: will resign active`, then
   the process's gateway socket closed two minutes later with no log line;
   the launches that followed wrote nothing at all — not even the app's
   first log line — until the phone was rebooted. So those launches hung
   before the app's own code ran, in a startup step that talks to a
   system daemon (CallKit provider registration, the audio session, the
   Local Push manager, or the SIP stack's audio unit), and a wedged daemon
   is the common factor with the loop spin. No MetricKit report had
   arrived by the next launch. Added: launch breadcrumbs around each of
   those steps (`Breadcrumb.drop`, in the uploaded log), a 20 s bound on
   the SIP stack start (`cb_start` returns ETIMEDOUT instead of freezing
   the app), and `active=` (NEAppPushManager.isActive) in the Local Push
   line — the extension had not connected all evening, which is why the
   incoming call at 00:37 found "callee offline, wake undeliverable".
   Next time: the last `launch:` breadcrumb in `data/diag/` names the
   hanging step; an Analytics `.ips` with `0x8badf00d` (launch watchdog)
   would show the blocked call directly.
9. **CLOSED — "app silent after a wake, then launches that hang and cannot
   be killed" (2026-09-13 06:13 SAST; same shape as the 00:37 episode and
   the 2026-09-12 "restart needed a reboot"): the app was running under
   Xcode's debugger.** The device log archive settled it: the process
   carried `debug:asserted` from the start and `debugserver` held the
   adjacent pid (1228/1229, a Wi-Fi debug session). At 06:13:08 the
   extension's wake reached the system, callservicesd launched the app
   to deliver it (`Successfully launched application`), SpringBoard
   forwarded the scene event with a 30 s watchdog — and the process never
   ran a thread: watchdogd reported "Impacted by suspension of Dialler",
   "Best option is process being debugged". A debuggee stopped by lldb
   (breakpoint, signal, EXC_RESOURCE — a CPU-limit exception goes to the
   debugger first, which is what the 2026-09-12 loop spin would have
   produced under Xcode) has its task suspended: its main thread cannot
   take the PushKit delivery, so callservicesd killed it at 06:13:16 with
   0xBAADCA11 — `terminate_with_reason() success`, yet a suspended task
   does not exit until it is resumed, so pid 1228 stayed. The user's tap
   at 06:13:29 foregrounded the frozen process (FrontBoard: "Ignoring
   watchdog … process is being debugged", "Not terminating … process is
   being debugged"); two swipe-kills at 06:13:44 and 06:13:57 were
   accepted and never completed ("Still waiting on exit context after
   19.1 seconds"). The process finally disappeared at 06:36:36, the
   moment debugserver 1229 exited (its connection to the Mac reset).
   Nothing of ours could log any of this, and no crash report is written
   for a debugged process, which is why every launch "left no trace".
   *Not determined:* what stopped the debuggee at ~06:12; Xcode's debug
   console at the time would have said (an EXC_RESOURCE from the loop
   spin, item 8, is the one candidate with prior evidence — the build's
   patch level, now printed by `cb_version()`, will say whether the guard
   was present). *Rule for field testing:* never leave a device run under
   the debugger. Install with Xcode, stop, launch from the home screen,
   or untick "Debug executable" in the scheme's Run action; a debugged
   app cannot be woken by CallKit once stopped and cannot be killed
   without a reboot or a dead debug link. *Checklist item:* a log
   archive's `debug:asserted` on the app's process state, or FrontBoard's
   "process is being debugged", ends the investigation before it starts.
   Seen beside it on every wake, successful ones included:
   nesessionmanager "failed to report incoming call to CallKit … Code=4099
   com.apple.callkit.networkextension.messagecontrollerhost was
   invalidated" — callservicesd still launches the app; treated as noise.

Retired to §6 "Much later" with their features: Wi-Fi → cellular handoff on
the SIP leg, and public-edge exposure to internet scanners.
