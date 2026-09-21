import DiallerProtocol
import Foundation

/// Pure protocol-session logic shared by every transport: handshake state,
/// heartbeat scheduling, wake de-duplication, and translation of envelopes
/// into `SignalEvent`s. No I/O, so it is fully unit-tested.
public struct SessionMachine: Equatable {
    public enum State: Equatable {
        case idle
        case awaitingWelcome
        case live(sessionID: String, heartbeatSeconds: Int)
        case closed
    }

    public enum Action: Equatable {
        case emit(SignalEvent)
        case send(Message)
        /// Re-run `evaluateLiveness()` in this many seconds.
        case scheduleLivenessCheck(seconds: Int)
        case close
    }

    /// Clock differences smaller than this are left alone (network latency,
    /// normal NTP drift); larger ones are treated as skew and corrected.
    public static let skewTolerance: TimeInterval = 5

    public private(set) var state: State = .idle
    /// call_ids already surfaced, so a replayed wake after reconnect does not
    /// ring twice (PROTOCOL.md §6). Pruned by expiry.
    private var seenWakes: [String: Date] = [:]
    /// When the server last said anything. A healthy link carries at least
    /// one frame per heartbeat interval (the pong to our ping), so silence
    /// for `idleMultiple` intervals means the connection is dead even if
    /// TCP has not noticed: the phone's Wi-Fi dropped, or the server closed
    /// while we were asleep. Mirrors the server's own 3 × heartbeat rule
    /// (PROTOCOL.md §5).
    private var lastReceived: Date
    public static let idleMultiple = 3
    private let now: () -> Date

    public init(now: @escaping () -> Date = Date.init) {
        self.now = now
        lastReceived = now()
    }

    public static func == (lhs: SessionMachine, rhs: SessionMachine) -> Bool {
        lhs.state == rhs.state && lhs.seenWakes == rhs.seenWakes
    }

    /// Transport opened: send hello.
    public mutating func didOpen(hello: Hello) -> [Action] {
        state = .awaitingWelcome
        lastReceived = now()
        return [.send(.hello(hello))]
    }

    /// Transport closed for any reason.
    public mutating func didClose(reason: String) -> [Action] {
        guard state != .closed else { return [] }
        state = .closed
        return [.emit(.disconnected(reason: reason))]
    }

    /// Is the link still alive? Silence for `idleMultiple` heartbeat
    /// intervals means it is dead even if TCP has not noticed — the phone's
    /// Wi-Fi dropped, or the server closed while we were asleep — so the
    /// keeper is told to reconnect. Without this a lost connection lived
    /// until TCP gave up, many minutes on a sleeping phone (the extension's
    /// 2026-09-13 stalls).
    ///
    /// It never sends anything. The heartbeat is the server's to originate
    /// (PROTOCOL.md §5, SPEC §4.7): a timer of ours may simply not fire in
    /// the Local Push extension, which is what made us look dead while we
    /// were fine. This only ever *reads the clock*, so it is correct however
    /// irregularly it is called — which is what lets the extension drive it
    /// from `handleTimerEvent()`, the system's own callback, exactly as
    /// Apple's sample does (`example/`, SimplePushKit `HeartbeatMonitor`).
    public mutating func evaluateLiveness() -> [Action] {
        guard case .live(_, let hb) = state else { return [] }
        let limit = TimeInterval(Self.idleMultiple * hb)
        if now().timeIntervalSince(lastReceived) > limit {
            state = .closed
            return [.emit(.disconnected(reason: "no frame from the server within \(Int(limit))s")), .close]
        }
        return [.scheduleLivenessCheck(seconds: hb)]
    }

    /// A frame arrived.
    public mutating func received(_ envelope: Envelope) -> [Action] {
        pruneSeenWakes()
        lastReceived = now()
        switch (state, envelope.message) {
        case (.awaitingWelcome, .welcome(let w)):
            state = .live(sessionID: w.sessionID, heartbeatSeconds: w.heartbeatSeconds)
            return [.emit(.connected(w)), .scheduleLivenessCheck(seconds: w.heartbeatSeconds)]

        case (.awaitingWelcome, .error(let e)):
            state = .closed
            return [.emit(.protocolError(e)), .emit(.disconnected(reason: "server: \(e.code.rawValue)")), .close]

        case (.awaitingWelcome, _):
            // Server MUST NOT send anything else before welcome; ignore.
            return []

        case (.live, .wake(var w)):
            // Rebase the ring deadline onto our clock. `expires_at` is server
            // time; the server and the phone may disagree by minutes (Docker
            // VM drift, unsynced on-prem boxes). What the server means is
            // "ring for (expires_at − ts) from now", so apply that duration
            // to our own clock rather than trusting the absolute stamp.
            let skew = now().timeIntervalSince(envelope.ts)
            if abs(skew) > Self.skewTolerance {
                w.expiresAt = w.expiresAt.addingTimeInterval(skew)
            }
            if let seen = seenWakes[w.callID], seen > now() {
                return [] // duplicate (replay after reconnect)
            }
            seenWakes[w.callID] = w.expiresAt
            return [.emit(.wake(w))]

        case (.live, .wakeCancel(let c)):
            return [.emit(.wakeCancel(c))]

        case (.live, .directoryChanged(let d)):
            return [.emit(.directoryChanged(d.version))]

        case (.live, .config(let c)):
            return [.emit(.config(c))]

        case (.live, .error(let e)):
            // A fatal error is a drop: the server closes right after it, and
            // the close is what the keeper acts on. Until 2026-09-13 only
            // `.close` followed, so the connection was torn down without a
            // `.disconnected` and nothing ever reconnected — the extension
            // logged "gateway error idle_timeout fatal=true" and then sat
            // silent for the rest of the night.
            if e.fatal {
                state = .closed
                return [.emit(.protocolError(e)), .emit(.disconnected(reason: "server: \(e.code.rawValue)")), .close]
            }
            return [.emit(.protocolError(e))]

        case (.live, .ping):
            return [.send(.pong)]

        case (.live, .pong), (.live, .hello), (.live, .welcome), (.live, .wakeAck), (.live, .unknown):
            return [] // unknown / wrong-direction types are ignored (PROTOCOL.md §2)

        case (.idle, _), (.closed, _):
            return []
        }
    }

    private mutating func pruneSeenWakes() {
        let t = now()
        seenWakes = seenWakes.filter { $0.value > t }
    }
}
