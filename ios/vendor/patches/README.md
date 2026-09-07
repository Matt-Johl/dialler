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
