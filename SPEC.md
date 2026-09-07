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
Video and IM are planned later phases.

## 2. Core mechanism (the load-bearing decision)

Wakeups use Apple's **Local Push Connectivity (LPC)** via the Network Extension
framework: an **App Push Provider** extension (`NEAppPushProvider`, managed by
`NEAppPushManager`) maintains a persistent TLS connection to an on-prem server
and reports incoming calls directly to **CallKit**. This is the only
Apple-sanctioned wakeup that does not route through APNS.

### Hard constraints this imposes
- **Wi-Fi-network-bound.** The push provider only runs while the device is
  joined to a designated Wi-Fi SSID (`matchSSIDs`). Off that network there are
  no LPC wakeups. This matches the on-prem goal ("works even if internet is
  down, as long as local Wi-Fi is up") but means it is **not** whole-world
  coverage. Remote users (cellular / home Wi-Fi) are Phase 4b via an
  APNS/PushKit sibling transport — see §4.6.
- **App Store distribution.** SSID configuration is done in-app through
  `NEAppPushManager` (no MDM). Requires the Network Extension entitlement
  (`com.apple.developer.networking.networkextension` → `app-push-provider`).
  App Review will scrutinize Network Extension usage; justify the on-prem use
  case clearly.
- **Extension is resource-limited.** Keep the extension to signaling only — no
  media. Media runs in the main app once foregrounded by CallKit.

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
| SIP + media stack | **baresip / libre / librem** (BSD-3), **Opus** (BSD) — fully permissive, commercializable, no license fee |
| PBX target | **Asterisk** for dev/test; compatible with **Cisco CUCM** and general SIP exchanges → server is a standard SIP element, PBX-agnostic. **PBX is optional** — the server also routes app↔app calls directly with no PBX present |
| Exchange model | **Own exchange.** The light server is the call controller. Any PBX is a SIP trunk peer only. The app never registers to a PBX, in dev or prod (§4.4) |
| App↔server leg | **SIP (baresip) under a strict private profile** (§4.4). Reversible to WebRTC media + wire-protocol signalling at Phase 5 without touching trunk, wake, or directory (§4.5) |
| Wire framing | **Length-prefixed JSON over TLS 1.3** (port 7443) for every signal transport: foreground LAN socket, LPC extension socket, public edge. No WebSocket — raw TLS is the natural `NWConnection` fit for the extension. Frozen in [protocol/PROTOCOL.md](protocol/PROTOCOL.md) with golden fixtures shared by Go and Swift |
| Server language | **Go**, standard library only (no external modules) |
| Coverage | **Wi-Fi-only (LPC)** initially; **remote app users** (cellular / home Wi-Fi) is a planned phase (4b) via APNS wake + a public server edge (§4.6). Wake transport abstracted so this adds a sibling, not a rework |
| Distribution | **Public App Store**, in-app LPC configuration |
| Video / IM | Designed-for, out of early scope (WebRTC/BSD for video; SIP MESSAGE/SIMPLE or XMPP for IM) |

## 4. Architecture

```
┌──────────────────────── iOS device (on-prem Wi-Fi) ─────────────────────────┐
│                                                                              │
│  Main App (SwiftUI)               App Push Provider Extension (NE target)     │
│  ├─ CallEngine  (SIP/RTP)         ├─ PushTransport (persistent TLS → server)  │
│  ├─ CallKit control               ├─ reports incoming call → CallKit          │
│  ├─ AddressBookStore              └─ foregrounds main app on answer           │
│  ├─ DirectorySync                                                             │
│  └─ AudioSession (Opus/G.711)     Shared framework (App Group):              │
│                                    protocol models, keychain, config          │
└─────────────────────────────────────┬────────────────────────────────────────┘
                                       │ TLS, transport-agnostic wake/signal
                                       ▼
                    ┌─────────────────────────────────────┐
                    │  Light Server (on-prem, Go or Rust)  │
                    │  ├─ Wake gateway (LPC conns; APNS    │
                    │  │   adapter Phase 4b)               │
                    │  ├─ Public edge (SIP/TLS + relay;    │
                    │  │   remote phase)                   │
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
  (LPC now, APNS from Phase 4b), waits for the app to register, then bridges
  the legs as a B2BUA — the same wake path used for PBX-originated calls.
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
assumptions baked in.

1. The app registers **only to the light server**. Never to a PBX — including
   Asterisk in dev.
2. App-leg SIP is **TLS over TCP only**. No UDP listener exists on the
   app-facing side.
3. App-leg media **always relays through the server**. ICE/TURN may be added
   later as an optimisation; the relay path is the baseline and must exist from
   Phase 0.
4. **SRTP mandatory.** Codecs: Opus primary, G.711 fallback.
5. Wake, directory, presence, and later IM travel on the **wire-protocol
   channel**. SIP carries call setup and media only.
6. **REFER (transfer) is handled by the server** as B2BUA, never passed through
   to the far leg.
7. The `PBXAdapter` speaks **trunk SIP and nothing else**. AMI/ESL remain
   dev-only.

### 4.5 Why SIP on the app leg, and when to replace it
SIP is kept on the app leg because baresip supplies SDP negotiation, codecs,
SRTP, jitter buffer, hold, and transfer for free; because the headless
baresip ↔ server ↔ baresip harness (§7.2) depends on both ends speaking SIP;
and because the B2BUA stays symmetric — one SIP stack for both legs.

It is reversible because the app sees the leg only through the `CallEngine`
protocol and the server terminates it in the B2BUA. Swapping to WebRTC media
with signalling on the wire protocol changes those two components only. The
PBX trunk, wake path, directory, and CallKit layer do not change.

Known weakness: mid-call Wi-Fi → cellular handoff uses re-register + re-INVITE
with the new media address, which is clunkier than a WebRTC ICE restart and
drops a second or two of audio.

Switch triggers: **(a)** Phase 5 video, when libwebrtc arrives anyway and can
carry audio too, removing baresip entirely; **(b)** handoff quality proves
unacceptable to remote users before then (measured in Phase 4b).

### 4.6 Remote users (off-prem app users)
Remote users are staff away from the site — on cellular or home Wi-Fi — who
must still receive and place calls. This is Phase 4b. Each addition maps to an
existing seam; none changes the call logic.

- **Wake:** `APNSTransport` (PushKit VoIP push). Must report to CallKit
  immediately on receipt or iOS penalises the app. Carries the same
  wire-protocol payload as LPC.
- **Public edge:** a SIP/TLS listener + media relay exposed to the internet —
  a session-border-controller role in the same binary. The PBX stays behind
  it and is **never** exposed.
- **Device auth:** a per-device credential issued at enrolment (token or client
  cert), with revocation. LAN trust is not sufficient.
- **Reconnect / resume:** the wire protocol carries session resume so a device
  leaving the SSID re-attaches over cellular; SIP re-registers and re-INVITEs
  for in-progress calls.

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
| 9 | **`AudioSession` / media** | Opus/G.711 capture/playback | integration / manual call tests |
| 10 | **Device enrolment / auth** (server + Swift) | issue / verify / revoke device credential | HTTP-level + keychain tests |

Key abstractions to keep future-proofing cheap (see §7 for how each is tested
without a device):
- `SignalTransport` protocol with three impls: `LANSocketTransport` (foreground
  LAN socket + automated-test sibling), `LPCTransport` (`NEAppPushProvider`,
  background survival), and `APNSTransport` (Phase 4b). The app/server treat a
  delivered message identically regardless of impl.
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
- **Phase 3** — Address book store + server-driven directory sync.
- **Phase 4** — `LPCTransport`: device-verify against the transport conformance
  suite, then flip background wakes to LPC.
- **Phase 4b** — **Remote users** (§4.6): `APNSTransport` via the same
  `SignalTransport` seam + public edge + enrolment auth + session resume.
  Measure Wi-Fi → cellular handoff quality here (§4.5 trigger b).
- **Phase 5** — Video (WebRTC/BSD, VideoToolbox). Decision point for moving
  audio to libwebrtc too (§4.5 trigger a).
- **Phase 6** — IM (SIP MESSAGE/SIMPLE or XMPP).

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
  differs (seam #2).
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
5. *(Phase 4b)* the APNS sibling passes the same conformance suite on-device.

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
| baresip / libre / librem | BSD-3 | SIP + RTP, modular C, production-grade |
| Opus | BSD | primary audio codec |
| G.711 / G.722 | royalty-free | interop fallback |
| G.729 | patents expired (~2017) | optional |
| WebRTC / libwebrtc (video, later) | BSD-3 | SRTP, ICE, codecs; VP8/VP9/AV1 royalty-free |
| Server runtime (Go or Rust) | permissive stdlib + deps | — |
| FreeSWITCH (candidate embedded engine) | MPL 1.1 | commercial on-prem distribution permitted; Phase 0 spike |
| sipgo / diago (candidate Go SIP) | MIT (verify) | maturity for transfer + CUCM unproven; Phase 0 spike |

Explicitly avoided: PJSIP / Linphone (GPL-or-paid; GPL conflicts with closed
App Store distribution). Kamailio, rtpengine, and Asterisk as *embedded*
engines (GPL) — Asterisk stays as the dev/test PBX peer, which is fine because
it is neither linked nor redistributed.

## 9. Top risks to validate early

1. **LPC entitlement + App Review** for a public App Store app — confirm
   `app-push-provider` is grantable for this use case and survives review.
2. **Extension longevity/resource limits** holding a persistent connection.
3. **CUCM SIP interop** quirks vs. Asterisk (third-party SIP device
   provisioning, registration behavior) — isolate behind `PBXAdapter`.
4. **baresip ↔ CallKit ↔ AVAudioSession** lifecycle on answer (cold launch from
   extension) — the trickiest integration seam.
5. **B2BUA build cost.** Attended transfer across two legs, re-INVITE glare,
   CUCM quirks. Mitigated by the Phase 0 engine spike.
6. **Wi-Fi → cellular handoff on the SIP leg.** Measure in Phase 4b; it is the
   trigger for replacing the app leg (§4.5).
7. **Public edge exposure.** SIP/TLS on the internet attracts scanners. Rate
   limiting, auth-before-INVITE, and no UDP (§4.4, §4.6).
