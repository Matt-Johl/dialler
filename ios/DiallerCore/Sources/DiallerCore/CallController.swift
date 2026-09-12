import DiallerProtocol
import Foundation

/// The system call UI as the controller sees it. Implemented by the CallKit
/// adapter in the app and by a fake in tests (SPEC §7.1 call-UI seam).
public protocol CallUI: AnyObject {
    /// Show an incoming call. Must be fast; on iOS it maps to
    /// `CXProvider.reportNewIncomingCall`.
    func reportIncoming(callID: String, displayName: String, handle: String, completion: @escaping (Error?) -> Void)
    /// A better caller name became known while the call is still ringing
    /// (the wake arrived after the INVITE had already rung it). On iOS it
    /// maps to `CXProvider.reportCall(with:updated:)`. Optional.
    func updateIncoming(callID: String, displayName: String)
    /// Remove a ringing/active call from the system UI.
    func end(callID: String, reason: CallEndReason)
    /// Ask the system to start an outgoing call to `handle` (a number or a
    /// SIP user). The system calls back through `CallController.userStarted`
    /// once it has approved the call (CallKit: CXStartCallAction).
    func startOutgoing(callID: String, handle: String, displayName: String)
    /// The far end is being alerted / has answered our outgoing call.
    func outgoingConnecting(callID: String)
    func outgoingConnected(callID: String)
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
    /// An INVITE arrived at the SIP stack; `peer` is the caller's URI and
    /// `displayName` the From display name if the caller sent one. The
    /// controller decides whether it belongs to a call already ringing
    /// (from a wake) or must ring the system UI itself.
    var onIncomingCall: ((_ peer: String, _ displayName: String?) -> Void)? { get set }
    /// The SIP call ended (remote hangup, failure); `reason` is free text.
    var onCallEnded: ((_ reason: String) -> Void)? { get set }
    /// Our outgoing call: the far end is ringing (180/183) / has answered.
    var onOutgoingRinging: (() -> Void)? { get set }
    var onCallEstablished: (() -> Void)? { get set }
    /// A transfer we asked for was refused; `reason` is the SIP status. The
    /// call continues.
    var onTransferFailed: ((_ reason: String) -> Void)? { get set }
    /// The device's enrolment credential, presented as SIP Digest username /
    /// password on the app leg (username = device id, password = token). The
    /// server refuses registrations and calls without it.
    func setCredentials(username: String, password: String)
    /// Register `user` (e.g. "201@dialler") to `sip` and stay registered, so
    /// calls reach this app directly while it runs. Idempotent.
    func register(user: String, sip: SIPTarget)
    /// Drop the current registration and its connection so the next
    /// `register` starts a fresh one: the gateway session came back after a
    /// drop, and the SIP connection died in the same suspension. A no-op
    /// during a call.
    func resetRegistration()
    /// The user accepted a call: make sure we are registered and answer the
    /// INVITE (now if it has already arrived, else as soon as it does).
    func prepareForIncomingCall(callID: String, user: String, sip: SIPTarget)
    /// Place a call to `target`: a full SIP URI, "user@domain", or a bare
    /// user / number, which the engine completes with the account's domain.
    func dial(callID: String, to target: String)
    func hangup(callID: String)
    func setMuted(_ muted: Bool)
    /// Hold (re-INVITE sendonly, audio stopped) / resume the current call.
    func setHeld(_ held: Bool)
    /// Blind transfer: ask the server to connect the far end to `target`
    /// and end our call (REFER; the server routes the target like a call).
    func transfer(callID: String, to target: String)
    /// The system audio session became available / was taken away (CallKit
    /// didActivate / didDeactivate, AVAudioSession interruptions). Engines
    /// with their own audio units restart or stop them here.
    func audioSessionActivated()
    func audioSessionDeactivated()
}

public extension CallEngine {
    func setCredentials(username _: String, password _: String) {}
    func resetRegistration() {}
    func audioSessionActivated() {}
    func audioSessionDeactivated() {}
    func setMuted(_: Bool) {}
    func setHeld(_: Bool) {}
    func transfer(callID _: String, to _: String) {}
}

public extension CallUI {
    func updateIncoming(callID _: String, displayName _: String) {}
}

/// One ringing or active call as the controller tracks it.
public struct TrackedCall: Equatable, Sendable {
    public enum Phase: Equatable, Sendable { case ringing, answered, ended }
    public enum Direction: Equatable, Sendable { case incoming, outgoing }
    public var wake: Wake
    public var phase: Phase
    /// The INVITE for this call has reached the SIP stack.
    public var sipArrived: Bool = false
    /// The caller name last given to the system UI for this call.
    public var reportedName: String = ""
    /// The server's call id from the wake, if one arrived: the id a decline
    /// or busy ack must carry. Nil for a call that rang from the INVITE
    /// alone (synthetic "sip-N" id), which the server never issued a wake
    /// for and would refuse an ack about.
    public var wakeCallID: String? = nil
    public var direction: Direction = .incoming
    /// Outgoing only: what the user asked to call.
    public var target: String = ""
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
        engine?.onIncomingCall = { [weak self] peer, name in self?.handle(sipIncoming: peer, displayName: name) }
        engine?.onCallEnded = { [weak self] reason in self?.handle(sipEnded: reason) }
        engine?.onOutgoingRinging = { [weak self] in self?.handle(outgoingRinging: ()) }
        engine?.onCallEstablished = { [weak self] in self?.handle(established: ()) }
        engine?.onTransferFailed = { [weak self] reason in
            self?.log("transfer refused: \(reason); the call continues")
            self?.onTransferFailed?(reason)
        }
    }

    /// The app is told when a transfer it asked for was refused.
    public var onTransferFailed: ((String) -> Void)?

    /// Resolves the friendly name to show for an incoming caller. The app
    /// layer sets this to look the caller up in the directory (which it owns);
    /// it is given the caller's URI and any display name the wake carried, and
    /// returns the name to display, or nil to keep the wake's own. Left nil,
    /// the caller's own display name (then the URI's user) is used.
    public var resolveDisplayName: ((_ uri: String, _ provided: String?) -> String?)?

    /// The name shown for a caller, on every ring path: the directory's (via
    /// `resolveDisplayName`), else the caller's own display name, else the
    /// bare number.
    private func callerName(for party: Party) -> String {
        let provided = party.displayName?.isEmpty == false ? party.displayName : nil
        return resolveDisplayName?(party.uri, provided) ?? provided ?? Self.numberPart(of: party.uri)
    }

    /// Blind transfer of an answered call to `target` (number, user or URI).
    /// On success the server ends our call once the target answers.
    public func transfer(callID: String, to target: String) {
        let t = target.trimmingCharacters(in: .whitespacesAndNewlines)
        let answered: Bool = lock.withLock { calls[callID]?.phase == .answered }
        guard answered, !t.isEmpty else {
            log("transfer of \(callID) to \(t) ignored: not an answered call")
            return
        }
        log("\(callID): transferring to \(t)")
        engine?.transfer(callID: callID, to: t)
    }

    // MARK: Outgoing calls

    /// The user wants to call `handle` (number, user, or SIP URI). Returns
    /// the call id; the system UI approves the call and calls back
    /// `userStarted`, which is when the engine dials.
    @discardableResult
    public func startCall(to handle: String, displayName: String? = nil) -> String? {
        let trimmed = handle.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !trimmed.isEmpty else { return nil }
        guard let account = lock.withLock({ self.account }) else {
            log("cannot call \(trimmed): no SIP account yet")
            return nil
        }
        let busy: Bool = lock.withLock { calls.contains { $0.value.phase != .ended } }
        if busy {
            log("cannot call \(trimmed): a call is already in progress")
            return nil
        }
        let id: String = lock.withLock { sipCallSeq += 1; return "out-\(sipCallSeq)" }
        let wake = Wake(callID: id, from: Party(uri: "sip:\(account.user)"), to: Party(displayName: displayName, uri: trimmed),
                        sip: account.sip, expiresAt: now().addingTimeInterval(120))
        lock.withLock { calls[id] = TrackedCall(wake: wake, phase: .ringing, sipArrived: true, direction: .outgoing, target: trimmed) }
        log("calling \(trimmed) as \(id)")
        ui.startOutgoing(callID: id, handle: trimmed, displayName: displayName ?? trimmed)
        return id
    }

    /// The system UI approved the outgoing call: dial now.
    public func userStarted(callID: String) {
        guard let call: TrackedCall = lock.withLock({ calls[callID] }), call.direction == .outgoing else { return }
        engine?.dial(callID: callID, to: call.target)
    }

    /// The system UI could not start the call (CallKit refused the action).
    public func startFailed(callID: String) {
        lock.withLock { calls[callID] = nil }
        log("outgoing call \(callID) refused by the system")
    }

    public func setMuted(_ muted: Bool) {
        engine?.setMuted(muted)
    }

    /// System UI held / resumed the call (CallKit CXSetHeldCallAction).
    public func setHeld(callID: String, _ held: Bool) {
        let known: Bool = lock.withLock { calls[callID]?.phase == .answered }
        guard known else {
            log("hold=\(held) for \(callID) ignored: not an answered call")
            return
        }
        log("\(callID): \(held ? "hold" : "resume")")
        engine?.setHeld(held)
    }

    private func handle(outgoingRinging _: Void) {
        guard let id = outgoingCallID() else { return }
        log("\(id): far end ringing")
        ui.outgoingConnecting(callID: id)
    }

    private func handle(established _: Void) {
        guard let id = outgoingCallID() else { return }
        lock.withLock { calls[id]?.phase = .answered }
        log("\(id): connected")
        ui.outgoingConnected(callID: id)
    }

    private func outgoingCallID() -> String? {
        lock.withLock { calls.first { $0.value.direction == .outgoing && $0.value.phase != .ended }?.key }
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
    public func handle(sipIncoming peer: String, displayName: String? = nil) {
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
        // Same naming as the wake path (directory → caller's own display name
        // → bare number), so the banner does not depend on which of the two
        // arrives first. The caller's own name is kept on the synthetic wake
        // for the in-call title.
        let from = Party(displayName: displayName?.isEmpty == false ? displayName : nil, uri: peer)
        let name = callerName(for: from)
        let id: String = lock.withLock { sipCallSeq += 1; return "sip-\(sipCallSeq)" }
        let wake = Wake(callID: id, from: from, to: Party(uri: "sip:\(account.user)"),
                        sip: account.sip, expiresAt: now().addingTimeInterval(60))
        lock.withLock { calls[id] = TrackedCall(wake: wake, phase: .ringing, sipArrived: true, reportedName: name) }
        log("incoming SIP call \(id) from \(name) (no wake)")
        ui.reportIncoming(callID: id, displayName: name, handle: peer) { [weak self] err in
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
            // A gateway drop (.disconnected) is deliberately NOT a call
            // event. A ringing wake does not depend on the session that
            // carried it: the extension's session may have delivered it
            // (PushKit), and the INVITE comes from the SIP registration the
            // engine makes on answer. Ending wake-only calls here killed the
            // background wake — the app's own stale session reports its
            // drop as it resumes for the PushKit wake, before the user has
            // answered (testSessionDropDoesNotEndAnExtensionDeliveredWake).
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
        let name = callerName(for: w.from)
        let existing: (id: String, reported: String)? = lock.withLock {
            if let c = calls[w.callID] { return (w.callID, c.reportedName) }
            // The INVITE for this call may already be ringing under a
            // synthetic id (registered app, wake arrived second): one call.
            // The INVITE knew only the peer URI; the wake carries the caller's
            // display name, so keep the richer identity for the in-call title.
            if let (id, c) = calls.first(where: { $0.value.sipArrived && $0.value.phase != .ended }) {
                var merged = c
                if merged.wake.from.displayName?.isEmpty != false { merged.wake.from.displayName = w.from.displayName }
                merged.wakeCallID = w.callID
                calls[id] = merged
                return (id, c.reportedName)
            }
            calls[w.callID] = TrackedCall(wake: w, phase: .ringing, reportedName: name, wakeCallID: w.callID)
            return nil
        }
        if let existing { // de-duplicated across app + extension delivery
            // A wake that resolves to a better name than the INVITE managed
            // corrects the banner while the call is still ringing.
            if name != existing.reported {
                lock.withLock { calls[existing.id]?.reportedName = name }
                log("call \(existing.id): caller now known as \(name)")
                ui.updateIncoming(callID: existing.id, displayName: name)
            }
            return .duplicate(of: existing.id)
        }
        if w.expiresAt <= now() {
            lock.withLock { calls[w.callID] = nil }
            log("wake \(w.callID) already expired; ignoring")
            return .expired
        }
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
        // Declining a ringing incoming call: tell the server through the
        // wake channel too (it may be holding the caller for our
        // registration), using the wake's own id. A call that rang from the
        // INVITE alone has no wake to ack; the 486 the engine sends is the
        // whole answer.
        let declineAckID: String? = lock.withLock {
            guard let c = calls[callID] else { return nil }
            calls[callID] = nil
            return c.phase == .ringing && c.direction == .incoming ? c.wakeCallID : nil
        }
        if let declineAckID {
            transport?.send(.wakeAck(WakeAck(callID: declineAckID, action: .decline)))
        }
        engine?.hangup(callID: callID)
        log("ended \(callID) by user")
    }

    /// "sip:201@dialler;transport=tls" → "201@dialler"; "201" → "201".
    public static func userPart(of uri: String) -> String {
        var s = uri
        if let lt = s.firstIndex(of: "<") { s = String(s[s.index(after: lt)...]) }
        if let gt = s.firstIndex(of: ">") { s = String(s[..<gt]) }
        for p in ["sips:", "sip:"] where s.hasPrefix(p) { s.removeFirst(p.count) }
        if let semi = s.firstIndex(of: ";") { s = String(s[..<semi]) }
        return s
    }

    /// The bare number/extension shown when there is no name: the user token
    /// before the host. "\"Alice\" <sip:1001@pbx>" → "1001"; "201" → "201".
    /// Unlike `userPart` this drops the host, so it is for display only, never
    /// for registering the SIP user (which needs the domain).
    public static func numberPart(of uri: String) -> String {
        let up = userPart(of: uri) // "1001@pbx" or "1001"
        return up.split(separator: "@").first.map(String.init) ?? up
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
