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
