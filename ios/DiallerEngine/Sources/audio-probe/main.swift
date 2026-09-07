// audio-probe: does the audiounit driver move audio under the CallKit
// contract? No SIP, no server: the driver's player + source are allocated
// through baresip's device layer and the frame counters are asserted.
//
// Scenarios (each must PASS):
//   A  hold → allocate → nothing flows → release → frames flow
//      (call established before didActivate)
//   B  release → allocate → frames flow
//      (didActivate before the call established — the ordering the app
//       usually sees, and the one the first driver patch missed)
//   C  hold while running → frames stop; release → frames resume
//      (didDeactivate / interruption, then a new activation)
//
// Output frames must flow in every case. Input frames need microphone
// access; a run without it reports "rec=0 (no mic access)" and still
// passes on output, because the failure this guards was units that never
// rendered at all.
//
//   make audio-probe        # macOS (HAL output unit)
//   make audio-probe-sim    # iOS simulator (VoiceProcessingIO, as on device)
import CBaresip
import Foundation

setvbuf(stdout, nil, _IOLBF, 0)

let config = """
sip_listen              0.0.0.0:0
audio_player            audiounit,default
audio_source            audiounit,default
audio_alert             none
audio_srate             48000
audio_channels          1
module                  audiounit.so
module                  auconv.so
module                  auresamp.so
"""

var failures = 0

func log(_ s: String) { print("probe: \(s)") }

func fail(_ s: String) {
    failures += 1
    print("FAIL: \(s)")
}

func stats() -> (play: UInt64, rec: UInt64) {
    var p: UInt64 = 0, r: UInt64 = 0, e: UInt64 = 0
    cb_audio_stats(&p, &r, &e)
    return (p, r)
}

/// Frames delivered during `seconds` from now.
func flow(over seconds: Double) -> (play: UInt64, rec: UInt64) {
    let a = stats()
    Thread.sleep(forTimeInterval: seconds)
    let b = stats()
    return (b.play - a.play, b.rec - a.rec)
}

let rc = cb_start(config, { _, ev, _, text in
    if ev == CB_EVENT_LOG, let text, String(cString: text).contains("audiounit") {
        print("  [driver] \(String(cString: text))")
    }
}, nil)
guard rc == 0 else {
    print("FAIL: cb_start \(rc)")
    exit(1)
}
log("stack up (\(String(cString: cb_version())))")

var micSeen = false

// A: established before activation.
log("A: hold, allocate, expect silence, release, expect flow")
cb_audio_interrupt(true)
var arc = cb_audio_test_alloc()
if arc != 0 { fail("A: alloc \(arc)") }
var f = flow(over: 1.0)
if f.play != 0 { fail("A: \(f.play) output frames flowed while held") }
cb_audio_interrupt(false)
f = flow(over: 1.5)
log("A: after release play=\(f.play) rec=\(f.rec)")
if f.play == 0 { fail("A: no output frames after release") }
micSeen = micSeen || f.rec > 0
cb_audio_test_free()

// B: activation before establishment.
log("B: release first, allocate, expect flow")
cb_audio_interrupt(false)
arc = cb_audio_test_alloc()
if arc != 0 { fail("B: alloc \(arc)") }
f = flow(over: 1.5)
log("B: play=\(f.play) rec=\(f.rec)")
if f.play == 0 { fail("B: no output frames when allocated on a released session") }
micSeen = micSeen || f.rec > 0

// C: hold while running, then release again.
log("C: hold while running, expect stop, release, expect resume")
cb_audio_interrupt(true)
_ = flow(over: 0.3) // let in-flight callbacks drain
f = flow(over: 1.0)
if f.play != 0 { fail("C: \(f.play) output frames flowed while held") }
cb_audio_interrupt(false)
f = flow(over: 1.5)
log("C: after re-release play=\(f.play) rec=\(f.rec)")
if f.play == 0 { fail("C: no output frames after re-release") }
micSeen = micSeen || f.rec > 0
cb_audio_test_free()

cb_stop()

if !micSeen { log("rec=0 throughout (no mic access in this environment); output asserted only") }
if failures == 0 {
    print("PASS: audiounit driver starts only on release and moves audio in every ordering")
    exit(0)
} else {
    print("FAIL: \(failures) assertion(s)")
    exit(1)
}
