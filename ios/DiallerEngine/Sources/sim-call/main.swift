// sim-call: the app's whole call path, headless, on the iOS simulator.
//
// Same objects the app uses — LANSocketTransport → CallController →
// BaresipCallEngine (real baresip, real audiounit driver on
// VoiceProcessingIO) — with CallKit replaced by a scripted stand-in that
// answers and then activates the audio session in the order the device
// does. The call is asserted on what the phone actually received: RTP
// packets and the energy of the rendered audio (the caller plays a tone).
//
//   sim-call <gateway-host> <port> <device-id> <token> <wait-seconds> [activate-delay-ms]
//
// Prints "sim: PASS" or "sim: FAIL: <why>" and exits non-zero on failure.
// Driven by harness/sim_call.sh (`make sim-call`).
import CBaresip
import DiallerCore
import DiallerEngine
import DiallerProtocol
import Foundation

setvbuf(stdout, nil, _IOLBF, 0)

let args = CommandLine.arguments
guard args.count >= 6 else {
    print("usage: sim-call <host> <port> <device-id> <token> <wait-seconds> [activate-delay-ms]")
    exit(2)
}
let host = args[1]
let port = UInt16(args[2]) ?? 7443
let deviceID = args[3]
let token = args[4]
let waitSeconds = Int(args[5]) ?? 40
/// CallKit activates the session shortly after the answer action is
/// fulfilled; on the device that is ~100-200 ms, usually before the SIP
/// call establishes. Larger values test the other ordering.
let activateDelayMs = args.count > 6 ? (Int(args[6]) ?? 150) : 150
/// Optional audio source spec ("aufile,/path/tone.wav"): makes the simulated
/// phone transmit even though simctl-spawned processes have no microphone,
/// which is what drives the server's symmetric-RTP latching on a real phone.
let sourceOverride: String? = args.count > 7 && !args[7].isEmpty ? args[7] : nil
/// How long the "user" lets it ring before tapping Answer. A real person
/// takes seconds; the caller is already streaming into the server by then.
let answerDelayMs = args.count > 8 ? (Int(args[8]) ?? 800) : 800
/// Calls to take in this one process before reporting. The app takes many
/// calls per launch; a failure that appears only on the second call needs
/// the same engine, registration and session to be reused.
let callsWanted = args.count > 9 ? max(1, Int(args[9]) ?? 1) : 1
/// "nogateway": register over SIP only, no gateway session, so no wake ever
/// arrives and every call is rung from the INVITE (the app's "sip-N" path).
let noGateway = args.count > 10 && args[10] == "nogateway"
/// "outbound:<target>": this phone places the call (keypad / directory
/// path) instead of waiting for one; the harness callee auto-answers and
/// plays its tone, which is asserted exactly as for incoming calls.
let outboundTarget: String? = args.count > 10 && args[10].hasPrefix("outbound:") ? String(args[10].dropFirst("outbound:".count)) : nil
/// Hold test: after the 2 s verdict, hold for this long, then resume and
/// check that media stopped while held and flows again after. 0 = no hold.
let holdMs = args.count > 11 ? (Int(args[11]) ?? 0) : 0
/// Transfer test: after the 2 s verdict, REFER the far end to this target
/// and expect our call to end with "Call transfered" (the server connected
/// the other party and released us).
let transferTo: String? = args.count > 12 && !args[12].isEmpty ? args[12] : nil
/// Decline test: reject each call from the (scripted) CallKit banner after
/// `answerDelayMs` of ringing instead of answering it — the red button. The
/// caller must then get 486 Busy Here, which sim_call.sh asserts on phone-b's
/// log; here we assert the call rang, was never established, and ended.
let declineMode = args.count > 13 && args[13] == "decline"

/// RTP received on the current call so far (cumulative).
func rtpReceived() -> UInt32 {
    var m = cb_media_stats_t()
    cb_media_stats(&m)
    return m.rx_packets
}

let t0 = Date()
/// Lines carry seconds since start so a stall shows as a gap.
func stamp() -> String { String(format: "%7.3f", Date().timeIntervalSince(t0)) }
func out(_ s: String) { print("\(stamp()) sim: \(s)") }

let engine = BaresipCallEngine(acceptAnyCertificate: true)
engine.setCredentials(username: deviceID, password: token) // SIP Digest on the app leg
engine.audioSourceOverride = sourceOverride
var lastCloseReason = ""
engine.onCallEnded = nil // set by the controller below; we observe through the log instead
let engineLog: (String) -> Void = { line in
    print("\(stamp())   \(line)")
    if line.hasPrefix("engine: call closed (") {
        lastCloseReason = String(line.dropFirst("engine: call closed (".count).dropLast())
    }
}
engine.log = engineLog

let lock = NSLock()
var registered = false
var inCall = false
var ended = false
var engineFailed: String?

engine.onStateChange = { s in
    print("  [state] \(s)")
    lock.lock(); defer { lock.unlock() }
    switch s {
    case .registered: registered = true; out("registered")
    case .inCall: inCall = true; out("in call")
    case .failed(let why): engineFailed = why
    default: break
    }
}

/// Scripted CallKit: report → (user taps) answer → (CallKit) didActivate.
final class ScriptedCallKit: CallUI {
    var onAnswer: (String) -> Void = { _ in }
    var onDecline: (String) -> Void = { _ in }
    /// Calls reported so far and the one in progress, so the main loop can
    /// wait for the NEXT call rather than react to the previous one's end.
    var reported = 0
    var current: String?
    func reportIncoming(callID: String, displayName: String, handle: String, completion: @escaping (Error?) -> Void) {
        out("callkit: reporting \(callID) from \(displayName)")
        lock.lock(); reported += 1; current = callID; ended = false; lock.unlock()
        completion(nil)
        if declineMode {
            // The user taps the red button while it rings: CallKit's end
            // action → controller.userEnded → wake_ack{decline} on the wire
            // channel and a 486 on the SIP leg. No answer, no audio session.
            DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(answerDelayMs)) {
                out("callkit: end action for \(callID) while ringing (declined after \(answerDelayMs)ms)")
                self.onDecline(callID)
                lock.lock(); if self.current == callID { ended = true }; lock.unlock()
            }
            return
        }
        DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(answerDelayMs)) {
            out("callkit: answer action for \(callID) (after \(answerDelayMs)ms ringing)")
            self.onAnswer(callID) // → controller.userAnswered → engine answers → fulfill
            DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(activateDelayMs)) {
                out("callkit: didActivate (+\(activateDelayMs)ms)")
                engine.audioSessionActivated()
            }
        }
    }
    func end(callID: String, reason: CallEndReason) {
        out("callkit: ended \(callID) (\(reason))")
        lock.lock(); if current == callID { ended = true }; lock.unlock()
        engine.audioSessionDeactivated()
    }
    /// Outgoing: CallKit approves the start action at once, then activates
    /// the session shortly after (as on the device).
    var onStart: (String) -> Void = { _ in }
    func startOutgoing(callID: String, handle: String, displayName: String) {
        out("callkit: start action for \(callID) → \(handle)")
        lock.lock(); reported += 1; current = callID; ended = false; lock.unlock()
        onStart(callID)
        DispatchQueue.global().asyncAfter(deadline: .now() + .milliseconds(activateDelayMs)) {
            out("callkit: didActivate (+\(activateDelayMs)ms)")
            engine.audioSessionActivated()
        }
    }
    func outgoingConnecting(callID: String) { out("callkit: \(callID) connecting (far end ringing)") }
    func outgoingConnected(callID: String) {
        out("callkit: \(callID) connected")
        lock.lock(); inCall = true; lock.unlock()
        out("in call")
    }
}

/// One call's outcome.
struct CallResult {
    var index: Int
    var best: BaresipCallEngine.AudioVerdict?
    var established: Bool
    /// Decline mode: the call rang and then ended without being answered.
    var declined: Bool = false
}

func judge(_ r: CallResult) -> (String, Bool) {
    if declineMode {
        if r.established { return ("FAIL: call was answered, not declined", false) }
        if !r.declined { return ("FAIL: decline never took effect (still ringing at the deadline)", false) }
        return ("PASS: rang, declined from the banner, never established (phone-b must show 486 Busy Here)", true)
    }
    return judge(r.best, established: r.established)
}

func judge(_ v: BaresipCallEngine.AudioVerdict?, established: Bool) -> (String, Bool) {
    if !established { return ("FAIL: call never established (no INVITE or answer failed)", false) }
    guard let v else { return ("FAIL: no audio verdict (call ended before 2s?)", false) }
    if v.playFrames == 0 { return ("FAIL: output unit rendered nothing", false) }
    if v.rtpRx == 0 { return ("FAIL: no RTP received from the server", false) }
    if !v.audible { return ("FAIL: RTP arrived but rendered audio is silent (energy/frame \(v.playEnergy / max(v.playFrames, 1)))", false) }
    // A process spawned by simctl has no microphone access, so no capture
    // and nothing to send is the norm here; reported, not failed.
    let mic = v.recFrames > 0 ? "mic captured \(v.recFrames) frames, rtp tx=\(v.rtpTx)" : "rtp tx=\(v.rtpTx) (file source; no mic under simctl spawn)"
    if v.rxLost > 0 || v.jbUnderflow > 0 || v.jbLate > 0 {
        return ("PASS with GAPS: tone rendered; lost=\(v.rxLost) late=\(v.jbLate) underflow=\(v.jbUnderflow); \(mic)", true)
    }
    return ("PASS: tone received and rendered, no loss; \(mic)", true)
}

let callKit = ScriptedCallKit()
let controller = CallController(ui: callKit, engine: engine, log: { print("\(stamp())   \($0)") })
callKit.onAnswer = { controller.userAnswered(callID: $0) }
callKit.onDecline = { controller.userEnded(callID: $0) }
callKit.onStart = { controller.userStarted(callID: $0) }

let cfg = AppConfig(gateway: GatewayEndpoint(host: host, port: port, acceptAnyCertificate: true), deviceID: deviceID, token: token)
// The same session keeper as the app: reconnects after a drop, so a server
// restart mid-run (sim_call.sh RESTART_SERVER=1) must be survived.
let transport = GatewaySession(endpoint: cfg.gateway)
controller.attach(transport: transport)
var welcomes = 0
let eventTask = Task {
    for await ev in transport.events {
        switch ev {
        case .waiting(let r): out("gateway waiting: \(r)")
        case .connected(let w):
            out("gateway connected: session \(w.sessionID)")
            welcomes += 1
            if welcomes > 1 {
                // As the app does: the SIP connection died with the old
                // session, so register afresh rather than refresh on it.
                engine.resetRegistration()
            }
            if let sip = w.sip {
                controller.setAccount(user: "\(sip.user)@\(sip.domain)", sip: SIPTarget(host: sip.host, port: sip.port, transport: sip.transport))
            } else {
                out("FAIL: welcome carries no SIP account for \(deviceID)")
            }
        case .wake, .wakeCancel: controller.handle(ev)
        case .protocolError(let e): out("gateway error \(e.code.rawValue) \(e.message ?? "")")
        case .disconnected(let r): out("gateway disconnected: \(r)")
        case .directoryChanged: break
        }
    }
}
out("connecting to \(host):\(port) as \(deviceID); activation delay \(activateDelayMs)ms; \(declineMode ? "DECLINE" : "answer") after \(answerDelayMs)ms; source \(sourceOverride ?? "microphone"); calls \(callsWanted)\(noGateway ? "; NO gateway (INVITE-only path)" : "")")
if noGateway {
    // The gateway's welcome normally names the SIP account; without it,
    // register the harness account directly. The controller then rings
    // every INVITE as a synthetic "sip-N" call, exactly as the app does
    // when a wake fails to arrive.
    controller.setAccount(user: "201@dialler", sip: SIPTarget(host: host, port: 5061, transport: "tls"))
} else {
    transport.connect(hello: cfg.hello(kind: .app))
}

// Each call: wait for its 5 s verdict, its end, or the deadline. Between
// calls the process stays registered and connected, like the app.
let deadline = Date().addingTimeInterval(TimeInterval(waitSeconds))
var results: [CallResult] = []
var fatal: String?
for index in 1...callsWanted {
    lock.lock(); inCall = false; ended = false; let base = callKit.reported; lock.unlock()
    engine.clearAudioVerdicts()
    if let target = outboundTarget {
        // Place the call once registered (the account arrives with the
        // welcome; registration follows within a second).
        while Date() < deadline {
            lock.lock(); let ready = registered; lock.unlock()
            if ready { break }
            Thread.sleep(forTimeInterval: 0.25)
        }
        Thread.sleep(forTimeInterval: 0.5)
        out("dialling \(target)")
        if controller.startCall(to: target) == nil { out("FAIL: could not start the call") }
    }
    // Wait for this call to be reported before judging anything.
    while Date() < deadline {
        lock.lock(); let seen = callKit.reported > base; let failed = engineFailed; lock.unlock()
        if seen || failed != nil { break }
        Thread.sleep(forTimeInterval: 0.25)
    }
    var sawVerdict = false
    var holdResult: String?
    var transferAsked = false
    var transferResult: String?
    while Date() < deadline {
        Thread.sleep(forTimeInterval: 0.25)
        if declineMode {
            // Nothing to measure: the call is over once the decline fires.
            lock.lock(); let done = ended; let failed = engineFailed; lock.unlock()
            if failed != nil { fatal = failed; break }
            if done { sawVerdict = true; break }
            continue
        }
        if let target = transferTo, !transferAsked, let v = engine.audioVerdicts.first(where: { $0.seconds == 2 }), v.rtpRx > 0 {
            transferAsked = true
            lock.lock(); let id = callKit.current ?? ""; lock.unlock()
            out("transfer: REFER \(id) → \(target)")
            controller.transfer(callID: id, to: target)
        }
        if transferAsked, transferResult == nil {
            lock.lock(); let done = ended; lock.unlock()
            if done {
                transferResult = lastCloseReason.contains("transfer") ? "PASS: call released after the transfer (\(lastCloseReason))" : "FAIL: call ended with \(lastCloseReason), not a completed transfer"
                out("transfer: \(transferResult!)")
                if transferResult!.hasPrefix("FAIL") { fatal = transferResult }
                sawVerdict = true
                break
            }
        }
        if holdMs > 0, holdResult == nil, let v = engine.audioVerdicts.first(where: { $0.seconds == 2 }), v.rtpRx > 0 {
            // Hold: our re-INVITE goes sendonly; the server must stop
            // sending to us. Resume: it must start again.
            lock.lock(); let id = callKit.current ?? ""; lock.unlock()
            out("callkit: hold action for \(id)")
            controller.setHeld(callID: id, true)
            Thread.sleep(forTimeInterval: 0.6) // let in-flight packets land
            let atHold = rtpReceived()
            Thread.sleep(forTimeInterval: TimeInterval(holdMs) / 1000)
            let duringHold = rtpReceived() - atHold
            out("callkit: resume action for \(id)")
            controller.setHeld(callID: id, false)
            Thread.sleep(forTimeInterval: 0.6)
            let atResume = rtpReceived()
            Thread.sleep(forTimeInterval: 1.5)
            let afterResume = rtpReceived() - atResume
            out("hold: received \(duringHold) packets while held (\(holdMs)ms), \(afterResume) in 1.5s after resume")
            if duringHold > 5 { holdResult = "FAIL: server kept sending \(duringHold) packets while we were on hold" }
            else if afterResume < 40 { holdResult = "FAIL: only \(afterResume) packets after resume" }
            else { holdResult = "PASS: hold stopped media, resume restored it" }
            out("hold: \(holdResult!)")
        }
        if engine.audioVerdicts.contains(where: { $0.seconds == 5 }) && (holdMs == 0 || holdResult != nil) { sawVerdict = true; break }
        lock.lock(); let done = ended; let failed = engineFailed; lock.unlock()
        if failed != nil { fatal = failed; break }
        if done { break }
    }
    if let r = holdResult, r.hasPrefix("FAIL") { fatal = r }
    lock.lock(); let established = inCall; lock.unlock()
    let verdicts = engine.audioVerdicts
    for v in verdicts {
        out("call \(index) verdict @\(v.seconds)s: \(v.verdict) play=\(v.playFrames) rec=\(v.recFrames) energy/frame=\(v.playEnergy / max(v.playFrames, 1)) rtp tx=\(v.rtpTx) rx=\(v.rtpRx) lost=\(v.rxLost) jitter=\(v.jitterMs)ms jbuf late=\(v.jbLate) underflow=\(v.jbUnderflow)")
    }
    // The caller's tone is a short file, so RTP may have stopped by 5 s;
    // judge on the window that saw the most.
    let best = verdicts.max { ($0.rtpRx, $0.playEnergy) < ($1.rtpRx, $1.playEnergy) }
    lock.lock(); let wasDeclined = ended; lock.unlock()
    let res = CallResult(index: index, best: best, established: established, declined: wasDeclined)
    results.append(res)
    let (line, _) = judge(res)
    out("call \(index): \(line)")
    if fatal != nil || Date() >= deadline { break }
    if index < callsWanted {
        // Let this call end (the script hangs up) before expecting the next.
        while Date() < deadline {
            lock.lock(); let done = ended; lock.unlock()
            if done { break }
            Thread.sleep(forTimeInterval: 0.25)
        }
        if !sawVerdict { out("note: call \(index) ended before its 5s verdict") }
        out("ready for call \(index + 1)")
    }
}

lock.lock(); let wasRegistered = registered; lock.unlock()
func result() -> (String, Int32) {
    if let fatal { return ("FAIL: engine failed: \(fatal)", 1) }
    if !wasRegistered { return ("FAIL: never registered", 1) }
    if results.count < callsWanted { return ("FAIL: only \(results.count) of \(callsWanted) calls happened before the deadline", 1) }
    var allOK = true
    for r in results where !judge(r).1 { allOK = false }
    if !allOK { return ("FAIL: see per-call lines above", 1) }
    if declineMode { return ("PASS: \(callsWanted) call(s) declined from the banner", 0) }
    return ("PASS: \(callsWanted) call(s) received and rendered", 0)
}
let (line, code) = result()
out(line)

engine.hangup(callID: "sim")
transport.disconnect()
eventTask.cancel()
// Independent exit watchdog: baresip teardown must not hang the process.
Thread.detachNewThread { Thread.sleep(forTimeInterval: 5); exit(code) }
engine.stop()
exit(code)
