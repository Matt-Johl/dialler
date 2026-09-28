import DiallerProtocol
import Foundation

/// The demo's call engine (see `Demo`): the far end is a script, and the
/// "audio" is an echo the app plays (`echoing`).
///
/// - An outgoing call rings after `ringDelay` and connects after
///   `answerDelay`; a call to `Demo.busyNumber` ends busy (SIP 486)
///   instead, so the controller plays the busy tone and Recents says so.
/// - `ringIncoming` rings the phone from `Demo.caller`, the way an INVITE
///   reaches an app in front.
/// - The echo runs only while a call is live and not held *and* CallKit has
///   activated the audio session — the same contract as the real engine,
///   which must never start audio before `didActivate`.
public final class DemoCallEngine: CallEngine, @unchecked Sendable {
    public var onIncomingCall: ((String, String, String?, String?) -> Void)?
    public var onCallEnded: ((String, String, Int) -> Void)?
    public var onOutgoingRinging: ((String, Bool) -> Void)?
    public var onCallEstablished: ((String) -> Void)?
    public var onTransferFailed: ((String, String) -> Void)?

    /// Whether the echo should be playing; the app starts or stops it.
    public var echoing: (Bool) -> Void = { _ in }
    /// The microphone's mute, which silences the echo.
    public var echoMuted: (Bool) -> Void = { _ in }
    public var log: (String) -> Void = { _ in }

    public static let ringDelay: TimeInterval = 1.5
    public static let answerDelay: TimeInterval = 4
    public static let incomingDelay: TimeInterval = 3

    /// Runs `work` after `seconds` on the main queue, where CallKit's and
    /// the app's own calls into the engine arrive. A seam for the tests.
    var schedule: (_ seconds: TimeInterval, _ work: @escaping () -> Void) -> Void = { seconds, work in
        DispatchQueue.main.asyncAfter(deadline: .now() + seconds, execute: work)
    }

    private struct Call {
        var live = false
        var held = false
    }

    private let lock = NSLock()
    private var calls: [String: Call] = [:]
    private var seq = 0
    private var sessionActive = false
    private var echoOn = false

    public init() {}

    // MARK: The scripted far end

    public func ringIncoming() {
        schedule(Self.incomingDelay) { [weak self] in
            guard let self else { return }
            let id = self.newCall("demo-in")
            self.log("demo: \(Demo.caller.name) is calling")
            self.onIncomingCall?(id, "sip:\(Demo.uri(Demo.caller.number))", Demo.caller.name, nil)
        }
    }

    public func dial(callID _: String, to target: String) -> String? {
        let id = newCall("demo-out")
        let busy = CallController.numberPart(of: target) == Demo.busyNumber
        log("demo: calling \(target)\(busy ? " (always busy)" : "")")
        schedule(Self.ringDelay) { [weak self] in
            guard let self, self.isTracked(id) else { return }
            self.onOutgoingRinging?(id, false)
        }
        schedule(Self.answerDelay) { [weak self] in
            guard let self, self.isTracked(id) else { return }
            if busy {
                self.drop(id)
                self.onCallEnded?(id, "busy", 486)
            } else {
                self.update(id) { $0.live = true }
                self.onCallEstablished?(id)
            }
        }
        return id
    }

    public func answer(engineCallID: String) -> Bool {
        guard isTracked(engineCallID) else { return false }
        update(engineCallID) { $0.live = true }
        return true
    }

    public func reject(engineCallID: String) { drop(engineCallID) }
    public func hangup(engineCallID: String) { drop(engineCallID) }

    public func setHeld(engineCallID: String, _ held: Bool) {
        update(engineCallID) { $0.held = held }
    }

    /// The far end takes the transfer and the call ends, as after a
    /// successful REFER.
    public func transfer(engineCallID: String, to target: String) {
        log("demo: transferring to \(target)")
        schedule(1) { [weak self] in
            guard let self, self.isTracked(engineCallID) else { return }
            self.drop(engineCallID)
            self.onCallEnded?(engineCallID, "transferred", 0)
        }
    }

    public func setMuted(_ muted: Bool) { echoMuted(muted) }

    public func audioSessionActivated() {
        lock.withLock { sessionActive = true }
        refreshEcho()
    }

    public func audioSessionDeactivated() {
        lock.withLock { sessionActive = false }
        refreshEcho()
    }

    /// Nothing to register with: the demo line is always "registered".
    public func register(user: String, sip _: SIPTarget) {
        log("demo: line \(user) ready")
    }

    // MARK: Calls

    private func newCall(_ prefix: String) -> String {
        lock.withLock {
            seq += 1
            let id = "\(prefix)-\(seq)"
            calls[id] = Call()
            return id
        }
    }

    private func isTracked(_ id: String) -> Bool { lock.withLock { calls[id] != nil } }

    private func update(_ id: String, _ change: (inout Call) -> Void) {
        lock.withLock { if calls[id] != nil { change(&calls[id]!) } }
        refreshEcho()
    }

    private func drop(_ id: String) {
        lock.withLock { calls[id] = nil }
        refreshEcho()
    }

    /// Echo while the session is active and a call is live and not held.
    private func refreshEcho() {
        let change: Bool? = lock.withLock {
            let want = sessionActive && calls.values.contains { $0.live && !$0.held }
            guard want != echoOn else { return nil }
            echoOn = want
            return want
        }
        if let change { echoing(change) }
    }
}
