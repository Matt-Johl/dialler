// engine-probe: headless conformance run of the real BaresipCallEngine on
// macOS against the docker server (SPEC §7.1 audio/engine seam, automated
// sibling). What the phone does on answer, minus CallKit:
//
//   swift run engine-probe <server-ip> [user@domain] [port] [seconds]
//
// Registers, then waits for an incoming call and answers it. Exit 0 if a
// registration was achieved (and, if a call arrived, it was established);
// exit 1 with baresip's own log otherwise. Pair with:
//   DIALLER_PUBLIC_HOST=<mac-ip> make harness-up
//   make engine-probe              (this)
//   make harness-ring-sim          (in another shell: 202 dials 201)
import DiallerCore
import DiallerEngine
import DiallerProtocol
import Foundation

// Line-buffer stdout: when redirected to a file (probe_call.sh) Swift's print
// is otherwise fully buffered and the driver sees nothing until exit.
setvbuf(stdout, nil, _IOLBF, 0)

let args = CommandLine.arguments
guard args.count >= 2 else {
    FileHandle.standardError.write(Data("usage: engine-probe <server-ip> [user@domain] [port] [seconds]\n".utf8))
    exit(2)
}
let host = args[1]
let user = args.count > 2 ? args[2] : "201@dialler"
let port = args.count > 3 ? Int(args[3]) ?? 5061 : 5061
let seconds = args.count > 4 ? Int(args[4]) ?? 30 : 30

let engine = BaresipCallEngine(acceptAnyCertificate: true)
var registered = false
var established = false
var closed = false
let lock = NSLock()

engine.log = { line in print("[engine] \(line)") }
engine.onStateChange = { state in
    print("[state] \(state)")
    lock.lock(); defer { lock.unlock() }
    switch state {
    case .registered: registered = true
    case .inCall: established = true
    default: break
    }
}

print("probe: registering \(user) via \(host):\(port)/tls, waiting up to \(seconds)s")
engine.prepareForIncomingCall(callID: "probe", user: user, sip: SIPTarget(host: host, port: port, transport: "tls"))
// No CallKit on macOS: the audio session is always available, so release
// the engine's manual-audio hold here (the app does this from didActivate).
engine.audioSessionActivated()

let deadline = Date().addingTimeInterval(TimeInterval(seconds))
while Date() < deadline {
    Thread.sleep(forTimeInterval: 0.25)
    lock.lock(); let done = established; lock.unlock()
    if done { Thread.sleep(forTimeInterval: 5); break } // let some media flow
}

lock.lock(); let ok = registered; let call = established; lock.unlock()
print(ok ? "probe: REGISTERED" : "probe: NOT registered within \(seconds)s")
print(call ? "probe: CALL ESTABLISHED (media ran ~5s)" : "probe: no call established (dial 201 during the wait to test)")
fflush(stdout)

// Teardown watchdog: the verdict above is already final; a hang in the C
// stack's shutdown must not turn a pass into a stall.
let exitCode: Int32 = ok ? 0 : 1
let watchdog = Thread {
    Thread.sleep(forTimeInterval: 5)
    print("probe: engine shutdown hung; exiting anyway")
    fflush(stdout)
    exit(exitCode)
}
watchdog.start()
engine.hangup(callID: "probe")
engine.stop()
exit(exitCode)
