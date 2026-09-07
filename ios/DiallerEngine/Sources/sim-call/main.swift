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

func out(_ s: String) { print("sim: \(s)") }

let engine = BaresipCallEngine(acceptAnyCertificate: true)
engine.audioSourceOverride = sourceOverride
engine.log = { print("  \($0)") }

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
    /// Calls reported so far and the one in progress, so the main loop can
    /// wait for the NEXT call rather than react to the previous one's end.
    var reported = 0
    var current: String?
    func reportIncoming(callID: String, displayName: String, handle: String, completion: @escaping (Error?) -> Void) {
        out("callkit: reporting \(callID) from \(displayName)")
        lock.lock(); reported += 1; current = callID; ended = false; lock.unlock()
        completion(nil)
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
}

/// One call's outcome.
struct CallResult {
    var index: Int
    var best: BaresipCallEngine.AudioVerdict?
    var established: Bool
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
let controller = CallController(ui: callKit, engine: engine, log: { print("  \($0)") })
callKit.onAnswer = { controller.userAnswered(callID: $0) }

let cfg = AppConfig(gateway: GatewayEndpoint(host: host, port: port, acceptAnyCertificate: true), deviceID: deviceID, token: token)
let transport = LANSocketTransport(endpoint: cfg.gateway)
controller.attach(transport: transport)
let eventTask = Task {
    for await ev in transport.events {
        switch ev {
        case .waiting(let r): out("gateway waiting: \(r)")
        case .connected(let w):
            out("gateway connected: session \(w.sessionID)")
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
out("connecting to \(host):\(port) as \(deviceID); activation delay \(activateDelayMs)ms; answer after \(answerDelayMs)ms; source \(sourceOverride ?? "microphone"); calls \(callsWanted)\(noGateway ? "; NO gateway (INVITE-only path)" : "")")
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
    // Wait for this call to be reported before judging anything.
    while Date() < deadline {
        lock.lock(); let seen = callKit.reported > base; let failed = engineFailed; lock.unlock()
        if seen || failed != nil { break }
        Thread.sleep(forTimeInterval: 0.25)
    }
    var sawVerdict = false
    while Date() < deadline {
        Thread.sleep(forTimeInterval: 0.25)
        if engine.audioVerdicts.contains(where: { $0.seconds == 5 }) { sawVerdict = true; break }
        lock.lock(); let done = ended; let failed = engineFailed; lock.unlock()
        if failed != nil { fatal = failed; break }
        if done { break }
    }
    lock.lock(); let established = inCall; lock.unlock()
    let verdicts = engine.audioVerdicts
    for v in verdicts {
        out("call \(index) verdict @\(v.seconds)s: \(v.verdict) play=\(v.playFrames) rec=\(v.recFrames) energy/frame=\(v.playEnergy / max(v.playFrames, 1)) rtp tx=\(v.rtpTx) rx=\(v.rtpRx) lost=\(v.rxLost) jitter=\(v.jitterMs)ms jbuf late=\(v.jbLate) underflow=\(v.jbUnderflow)")
    }
    // The caller's tone is a short file, so RTP may have stopped by 5 s;
    // judge on the window that saw the most.
    let best = verdicts.max { ($0.rtpRx, $0.playEnergy) < ($1.rtpRx, $1.playEnergy) }
    results.append(CallResult(index: index, best: best, established: established))
    let (line, _) = judge(best, established: established)
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
    for r in results where !judge(r.best, established: r.established).1 { allOK = false }
    if !allOK { return ("FAIL: see per-call lines above", 1) }
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
