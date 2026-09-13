# Vendor patches

Applied by `build-baresip.sh` on top of the extracted release sources
(`ios/vendor/src`, scratch) before building. Files here are full
replacements for the matching upstream file, pinned to the release in
`build-baresip.sh` (baresip v3.15.0). Re-check them when bumping.

## audiounit/ — manual audio for CallKit

Upstream's audiounit driver calls `AudioOutputUnitStart` inside
`audiounit_player_alloc` / `audiounit_recorder_alloc`: the player starts
the instant the call is established, that start reconfigures the audio
hardware for voice processing, and the recorder is then created in the
middle of that reconfiguration. Under CallKit the contract is the one
WebRTC exposes as `RTCAudioSession.useManualAudio` + `isAudioEnabled`:
the engine never starts audio on its own; the host starts it from
`provider(_:didActivate:)` and stops it from `didDeactivate`.

The patch:

- while held, alloc does not touch CoreAudio at all. Under CallKit,
  creating or initialising a unit on a deactivated session fails with
  `'!pri'` (AVAudioSession insufficient priority) — seen on every call
  after the first on a device, where the SIP call established before
  CallKit's activation. The unit is created, configured, initialised and
  started in the release handler instead;
- never starts a unit inside alloc. Each alloc calls
  `audiosess_request_start()`, which arms a zero-delay libre timer; all
  allocated units start together once the allocation chain has returned
  to the loop, so player and recorder are both initialised before either
  starts;
- remembers the host's hold (`audiosess_interrupt(true)`) even while no
  units exist. While held, nothing starts; `audiosess_interrupt(false)`
  starts everything allocated so far and lets later allocations start;
- counts frames delivered through both callbacks
  (`audiosess_stats`), so `audio-probe` and the app's self-check can
  assert that CoreAudio is actually moving audio.

Verified by `make audio-probe` (macOS, HAL unit) and
`make audio-probe-sim` (iOS simulator, VoiceProcessingIO), which drive
the driver through both event orderings and fail if no frames flow, and
end to end by `make sim-call`.

## apply-baresip.sh — `ua_refresh_register()`

baresip's `ua_register()` on an already registered user agent destroys
and re-creates its registration clients, and libre's client sends an
un-REGISTER for the same contact as it is destroyed. That un-REGISTER
lands after the new REGISTER, so the server ends up with no binding
(found by `make sim-call`). `ua_refresh_register()` re-sends on the
existing client (`sipreg_send`) instead; the engine uses it whenever it
needs a fresh registration on a live account (new gateway session, wake
with no INVITE pending).

## apply-baresip.sh — packet-loss concealment on the receive path

baresip 3.15's `aurecv_receive()` (`src/aureceiver.c`) is handed the
number of frames lost before each packet and discards it — the call to the
codec's PLC handler sits commented out under a "TODO: what if lostc > 1".
So a lost packet was 20 ms of silence whatever the codec: Opus's own
concealment and its in-band FEC were never used. Measured on the impaired
echo path (2 % loss, 30 ± 10 ms jitter): one audible gap per lost packet,
nine drops → nine gaps.

The patch conceals each lost frame before decoding the packet that
arrived: the frame right before it gets that packet, so `opus_decode_pkloss`
can decode the FEC it carries; earlier ones get a NULL packet (plain PLC);
at most five frames (100 ms) are concealed per hole; concealed frames take
evenly spaced timestamps across the hole. Codecs without a PLC handler
(G.711, G.722 until their own handlers land) are unchanged.

Two configuration facts the patch depends on, both pinned by
`StackConfigTests`: baresip's Opus module only asks the encoder for FEC
data and only decodes it when `opus_packet_loss` is set (it drives both
`OPUS_SET_PACKET_LOSS_PERC` and the decoder's `fec` flag); and Opus must be
mono (`opus_stereo no`, `opus_sprop_stereo no`) — stereo at 32 kbit/s puts
libopus in CELT mode, which has no in-band FEC. The harness phone
(`harness/baresip/Dockerfile`) applies this script too, so the headless
gates measure the same behaviour as the app.

## g722/ — G.722 without spandsp

Upstream's `modules/g722` codes through spandsp (LGPL), and its CMake
silently skips the module when spandsp is absent — which is why the app
had no wideband codec for PBX calls. Linking spandsp statically into the
XCFramework is ruled out by SPEC §8, so `g722/CMakeLists.txt` and
`g722/g722.c` replace the upstream files: same module, same 64 kbit/s
mode, same wire format, coded through the G.722 implementation WebRTC
carries in `modules/third_party/g722`, vendored under
`g722/webrtc/modules/third_party/g722/` (the path its own includes use).

Licence gate (checked before the code was written, SPEC §8): the three
source files carry Steve Underwood's dedication placing the implementation
in the public domain (with the 1993 CMU notice for the original
`g722_encode`/`g722_decode` derivation); WebRTC's own edits are under its
BSD-3 licence (`LICENSE` beside them); Chromium records it as
`LicenseRef-Public-Domain-SpanDSP` (`README.chromium`). Nothing LGPL.

Provenance: github.com/webrtc-sdk/webrtc, commit
`5bc7e6574baf2e8b1781b013db7b81f1752685f5`, files
`modules/third_party/g722/{g722_encode.c,g722_decode.c,g722_enc_dec.h}`.
SHA-256 of the vendored copies:

- `g722_encode.c` `dde9fe6fc12a9facf2f70c0c8c9eb3151ea3d5a7505fb15aa13dcdacc6a85cd2`
- `g722_decode.c` `ad930feb5e109d06d9ba047f339926f295da41dae3c4f00cf136aa84fd50e24d`
- `g722_enc_dec.h` `33606ddc73e46803176e0e5936ced4e8600c2e6e3209776bc224a95a22d5539b`

The RTP clock is 8000 while the audio is 16 kHz (RFC 3551 §4.5.2, an
error kept for compatibility): the module registers `srate 16000`,
`crate 8000`, 160 octets per 20 ms. The harness phone builds upstream's
module against the distro's spandsp shared library instead (an LGPL
dependency is fine in a test container). Verified by `make harness-trunk`
(`codec=G722` on both legs of every trunk call, gap-free recordings).

## plc/ + g711/ — packet-loss concealment for G.711 and G.722

baresip calls a codec's `plch` for every lost frame (with our aureceiver
patch above), but only the Opus module implements one: on a G.711 or
G.722 call every lost packet was 20 ms of silence. `plc/plc.c` is our own
concealment unit — pitch-period repetition with overlap-add and a fade,
after the method of ITU-T G.711 Appendix I, written from the description
with no third-party code — compiled into both sample-domain modules:

- `g711/g711.c` + `g711/CMakeLists.txt` replace upstream's module: the
  decoder keeps a `struct plc` per call, feeds every decoded frame to
  `plc_good()`, and `plch` fills a lost frame from `plc_fill()`;
- `g722/g722.c` does the same after the WebRTC decoder.

The unit keeps 60 ms of history, estimates the pitch (2.5–15 ms) by
normalised cross-correlation over the last 20 ms, repeats the last period
with the first quarter period overlap-added onto the history, attenuates
20 % per further lost frame and is silent after five (a long loss must not
buzz), and cross-fades the first 5 ms of the first good frame after a loss.
`make plc-test` (plc/plc_test.c, plain C on the host) checks a concealed
frame keeps the signal's energy and shape (error well below the signal),
the recovery frame is undamaged, and a long loss fades to silence. The
harness phone (harness/baresip/Dockerfile) compiles the same modules, so
`IMPAIR=1 make harness-echo` measures the same concealment the app has.

## Tried and reverted: jitter buffer counting a lost packet as a frame

The residual gap after concealment (one 20–40 ms dip per lost packet, any
codec) is the jitter buffer's refill: `jbuf_get` releases a packet only
while the buffer holds more than `wish` frames, and a loss leaves it one
short until another packet arrives, so the player runs dry for a frame
before the hole is concealed. Counting the missing packets as "holes"
inside the frame count (so the following packet is released on time) was
tried on 2026-09-12 and reverted: it released packets early, drained the
buffer, and the impaired echo went from 1–2 gaps per call to 4–13. The fix
would be concealment driven by the playout timeline (a decode on a timer
when the slot's packet has not arrived), which is a receive-path redesign,
not a counting change. Left open; the numbers are in SPEC §6.

## apply-re.sh — libre's main loop must not spin on EBADF

Upstream `re_main()` has a Darwin "workaround": when `fd_poll()` fails
with `EBADF` it retries at once, forever, without running timers. That is
the state the loop is in once its kqueue descriptor has become invalid
(the context torn down under it, or the descriptor number closed twice by
someone else), and on iOS it is the engine thread at 100 % CPU in the
background until the system kills the app: `cpu_resource_fatal`, 48 s at
99 %, on 2026-09-12 — the heaviest stack was the loop thread inside
`re_main` calling `kevent`. The shim's "loop died" recovery never ran
because `re_main` never returned. Patched: warn with the descriptor state
(`kqfd`, so the next occurrence says whether it was closed under the loop
or by a stray close elsewhere), retry eight times 10 ms apart, then return
EBADF; the loop thread treats an unasked return as dead and the engine
rebuilds the stack. Applied to the harness phone too.

## apply-re.sh — QoS: Apple net service type beside the DSCP

`udp_settos()`, `tcp_settos()` and `tcp_conn_settos()` set `IP_TOS` only.
On iOS the DSCP byte does not by itself pick the Wi-Fi access category;
the socket's `SO_NET_SERVICE_TYPE` does (`NET_SERVICE_TYPE_VO` for voice,
`_SIG` for signalling), and IPv6 sockets need `IPV6_TCLASS` for the DSCP.
The patch adds both under `#ifdef DARWIN`, mapping tos ≥ 184 (EF) to voice
and tos ≥ 96 (CS3) to signalling, ignoring failures (the other address
family, or an unsupported option). baresip applies `rtp_tos` to RTP/RTCP
and `sip_tos` to the SIP transports through these functions; the app's
profile sets 184 and 96 (plan Phase E, SPEC §4.4 rule 5a).

## apply-re.sh — patch level

`re_dialler_patchlevel()` is appended to libre's `main.c` and returns
`RE_PATCH_LEVEL` from apply-re.sh (1: EBADF guard; 2: + the QoS marks).
`cb_version()` prints it in the engine's "started baresip …" log line, so a
field report says which XCFramework the build carried — an app built from
between an XCFramework rebuild and a source fix has cost an afternoon
before. The reference is deliberately not weak: an app linked against an
XCFramework older than the patch fails to link rather than run without it.
Bump the level whenever a patch above changes.

## Not a patch: libre's context is bound to the initialising thread

Recorded here because it looked like a libre bug and nearly became a
patch. `libre_init()` binds the libre context to the calling thread
(thread-local, freed by a thread-exit destructor, and `re_global` for
every other thread). Initialising on the caller's thread and only running
`re_main()` on the loop thread works until the caller is a GCD worker
thread: libdispatch retires those when idle, the destructor frees the
context the loop is polling, and `re_main()` returns "unasked" — `EINVAL`
from `kevent`, or `0` with `re_unlock error` — seconds after a (re)start
from a dispatch queue. From then on every shim call times out (`ua_alloc
failed (-60)`, `-60` being `ETIMEDOUT` from the shim's own wait). The
shim (`ios/DiallerEngine/Sources/CBaresip/cbaresip.c`) now creates, runs
and tears down the stack on its loop thread; found and verified by
`RESTART_SERVER=1 make sim-call`.
