import DiallerProtocol
import Foundation

/// The system call UI as the controller sees it. Implemented by the CallKit
/// adapter in the app and by a fake in tests (SPEC §7.1 call-UI seam).
public protocol CallUI: AnyObject {
    /// Show an incoming call. Must be fast; on iOS it maps to
    /// `CXProvider.reportNewIncomingCall`.
    func reportIncoming(callID: String, displayName: String, handle: String, completion: @escaping (Error?) -> Void)
    /// Remove a ringing/active call from the system UI.
    func end(callID: String, reason: CallEndReason)
}

public enum CallEndReason: Equatable, Sendable {
    case remoteEnded
    case answeredElsewhere
    case unanswered
    case failed
}

/// What the app would do with the SIP stack once the user answers. Phase 1
/// (signalling + CallKit) logs; the baresip-backed engine plugs in here.
public protocol CallEngine: AnyObject {
    /// An INVITE arrived at the SIP stack; `peer` is the caller's URI. The
    /// controller decides whether it belongs to a call already ringing
    /// (from a wake) or must ring the system UI itself.
    var onIncomingCall: ((_ peer: String) -> Void)? { get set }
    /// The SIP call ended (remote hangup, failure); `reason` is free text.
    var onCallEnded: ((_ reason: String) -> Void)? { get set }
    /// Register `user` (e.g. "201@dialler") to `sip` and stay registered, so
    /// calls reach this app directly while it runs. Idempotent.
    func register(user: String, sip: SIPTarget)
    /// The user accepted a call: make sure we are registered and answer the
    /// INVITE (now if it has already arrived, else as soon as it does).
    func prepareForIncomingCall(callID: String, user: String, sip: SIPTarget)
    func hangup(callID: String)
    /// The system audio session became available / was taken away (CallKit
    /// didActivate / didDeactivate, AVAudioSession interruptions). Engines
    /// with their own audio units restart or stop them here.
    func audioSessionActivated()
    func audioSessionDeactivated()
}

public extension CallEngine {
    func audioSessionActivated() {}
    func audioSessionDeactivated() {}
}

/// One ringing or active call as the controller tracks it.
public struct TrackedCall: Equatable, Sendable {
    public enum Phase: Equatable, Sendable { case ringing, answered, ended }
    public var wake: Wake
    public var phase: Phase
    /// The INVITE for this call has reached the SIP stack.
    public var sipArrived: Bool = false
}

/// Turns gateway events into system-call-UI actions and acks, and user UI
/// actions into engine calls. Transport-agnostic: the same controller runs
/// behind the foreground socket, the extension socket, or a PushKit payload.
public final class CallController {
    private let ui: CallUI
    private let engine: CallEngine?
    private let lock = NSLock()
    private var calls: [String: TrackedCall] = [:]
    private var transport: SignalTransport?
    private var account: (user: String, sip: SIPTarget)?
    private var sipCallSeq = 0
    private let now: () -> Date
    private let log: (String) -> Void

    public init(ui: CallUI, engine: CallEngine? = nil, now: @escaping () -> Date = Date.init, log: @escaping (String) -> Void = { _ in }) {
        self.ui = ui
        self.engine = engine
        self.now = now
        self.log = log
        engine?.onIncomingCall = { [weak self] peer in self?.handle(sipIncoming: peer) }
        engine?.onCallEnded = { [weak self] reason in self?.handle(sipEnded: reason) }
    }

    public var activeCalls: [TrackedCall] { lock.withLock { Array(calls.values) } }

    /// Binds a transport so acks can be sent back on it.
    public func attach(transport: SignalTransport) {
        lock.withLock { self.transport = transport }
    }

    /// The server told us (in welcome) which SIP user this device is:
    /// register now so calls reach us directly while the app runs.
    public func setAccount(user: String, sip: SIPTarget) {
        lock.withLock { account = (user, sip) }
        log("SIP account \(user) via \(sip.host):\(sip.port); registering")
        engine?.register(user: user, sip: sip)
    }

    // MARK: SIP stack → UI

    /// An INVITE reached the SIP stack. If a wake already rang for this
    /// call, just note it; otherwise ring the system UI from the INVITE.
    public func handle(sipIncoming peer: String) {
        let ringing: TrackedCall? = lock.withLock {
            guard let (id, c) = calls.first(where: { $0.value.phase != .ended }) else { return nil }
            var updated = c
            updated.sipArrived = true
            calls[id] = updated
            return updated
        }
        if let ringing {
            log("INVITE from \(peer) for call \(ringing.wake.callID) (already \(ringing.phase))")
            return
        }
        guard let account = lock.withLock({ self.account }) else {
            log("INVITE from \(peer) but no SIP account is known; ignoring")
            return
        }
        let id: String = lock.withLock { sipCallSeq += 1; return "sip-\(sipCallSeq)" }
        let wake = Wake(callID: id, from: Party(uri: peer), to: Party(uri: "sip:\(account.user)"),
                        sip: account.sip, expiresAt: now().addingTimeInterval(60))
        lock.withLock { calls[id] = TrackedCall(wake: wake, phase: .ringing, sipArrived: true) }
        log("incoming SIP call \(id) from \(peer) (no wake)")
        ui.reportIncoming(callID: id, displayName: peer, handle: peer) { [weak self] err in
            guard let self, let err else { return }
            self.log("CallKit refused SIP call \(id): \(err)")
            self.lock.withLock { self.calls[id] = nil }
            self.engine?.hangup(callID: id)
        }
    }

    /// The SIP call ended on the far side.
    public func handle(sipEnded reason: String) {
        let ended: [String] = lock.withLock {
            let ids = calls.filter { $0.value.sipArrived || $0.value.phase == .answered }.map(\.key)
            for id in ids { calls[id] = nil }
            return ids
        }
        for id in ended {
            log("call \(id) ended by SIP: \(reason)")
            ui.end(callID: id, reason: .remoteEnded)
        }
    }

    // MARK: Gateway → UI

    /// Handle one signal event. Non-call events are ignored here.
    public func handle(_ event: SignalEvent) {
        switch event {
        case .wake(let w):
            handle(wake: w)
        case .wakeCancel(let c):
            end(callID: c.callID, reason: c.reason == .answeredElsewhere ? .answeredElsewhere : (c.reason == .timeout ? .unanswered : .remoteEnded))
        default:
            break
        }
    }

    /// What a wake did, so the delivery path (socket or PushKit) can meet
    /// its own obligations: a PushKit delivery must always be answered with
    /// a CallKit report, even when the call is already ringing.
    public enum WakeOutcome: Equatable, Sendable {
        /// Rang the system UI for `callID`.
        case rang
        /// Already ringing (or answered) under `callID` — the same wake by
        /// another path, or the INVITE that arrived first.
        case duplicate(of: String)
        /// Past its expiry; nothing rang.
        case expired
    }

    /// Entry point shared by the transport path and the PushKit path.
    @discardableResult
    public func handle(wake w: Wake) -> WakeOutcome {
        let existing: String? = lock.withLock {
            if calls[w.callID] != nil { return w.callID }
            // The INVITE for this call may already be ringing under a
            // synthetic id (registered app, wake arrived second): one call.
            if let (id, _) = calls.first(where: { $0.value.sipArrived && $0.value.phase != .ended }) { return id }
            calls[w.callID] = TrackedCall(wake: w, phase: .ringing)
            return nil
        }
        if let existing { return .duplicate(of: existing) } // de-duplicated across app + extension delivery
        if w.expiresAt <= now() {
            lock.withLock { calls[w.callID] = nil }
            log("wake \(w.callID) already expired; ignoring")
            return .expired
        }
        let name = w.from.displayName?.isEmpty == false ? w.from.displayName! : w.from.uri
        log("incoming call \(w.callID) from \(name)")
        ui.reportIncoming(callID: w.callID, displayName: name, handle: w.from.uri) { [weak self] err in
            guard let self else { return }
            if let err {
                self.log("CallKit refused call \(w.callID): \(err)")
                self.lock.withLock { self.calls[w.callID] = nil }
                self.transport?.send(.wakeAck(WakeAck(callID: w.callID, action: .busy)))
                return
            }
            // Reported to the system UI before any network work (PROTOCOL.md §6).
            self.transport?.send(.wakeAck(WakeAck(callID: w.callID, action: .willAnswer)))
        }
        return .rang
    }

    // MARK: UI → engine

    /// User accepted the call in the system UI.
    public func userAnswered(callID: String) {
        guard let call: TrackedCall = lock.withLock({
            guard var c = calls[callID], c.phase == .ringing else { return nil }
            c.phase = .answered
            calls[callID] = c
            return c
        }) else { return }
        let user = Self.userPart(of: call.wake.to.uri)
        log("answered \(callID); registering \(user) to \(call.wake.sip.host):\(call.wake.sip.port)")
        engine?.prepareForIncomingCall(callID: callID, user: user, sip: call.wake.sip)
    }

    /// User declined or hung up in the system UI.
    public func userEnded(callID: String) {
        let wasRinging: Bool = lock.withLock {
            guard let c = calls[callID] else { return false }
            calls[callID] = nil
            return c.phase == .ringing
        }
        if wasRinging {
            transport?.send(.wakeAck(WakeAck(callID: callID, action: .decline)))
        }
        engine?.hangup(callID: callID)
        log("ended \(callID) by user")
    }

    /// "sip:201@dialler;transport=tls" → "201@dialler"; "201" → "201".
    static func userPart(of uri: String) -> String {
        var s = uri
        if let lt = s.firstIndex(of: "<") { s = String(s[s.index(after: lt)...]) }
        if let gt = s.firstIndex(of: ">") { s = String(s[..<gt]) }
        for p in ["sips:", "sip:"] where s.hasPrefix(p) { s.removeFirst(p.count) }
        if let semi = s.firstIndex(of: ";") { s = String(s[..<semi]) }
        return s
    }

    private func end(callID: String, reason: CallEndReason) {
        let known: Bool = lock.withLock {
            guard calls[callID] != nil else { return false }
            calls[callID] = nil
            return true
        }
        guard known else { return }
        log("call \(callID) ended: \(reason)")
        ui.end(callID: callID, reason: reason)
        engine?.hangup(callID: callID)
    }
}

extension NSLock {
    func withLock<T>(_ body: () throws -> T) rethrows -> T {
        lock()
        defer { unlock() }
        return try body()
    }
}
