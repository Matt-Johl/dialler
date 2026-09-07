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
        case scheduleHeartbeat(seconds: Int)
        case close
    }

    /// Clock differences smaller than this are left alone (network latency,
    /// normal NTP drift); larger ones are treated as skew and corrected.
    public static let skewTolerance: TimeInterval = 5

    public private(set) var state: State = .idle
    /// call_ids already surfaced, so a replayed wake after reconnect does not
    /// ring twice (PROTOCOL.md §6). Pruned by expiry.
    private var seenWakes: [String: Date] = [:]
    private let now: () -> Date

    public init(now: @escaping () -> Date = Date.init) {
        self.now = now
    }

    public static func == (lhs: SessionMachine, rhs: SessionMachine) -> Bool {
        lhs.state == rhs.state && lhs.seenWakes == rhs.seenWakes
    }

    /// Transport opened: send hello.
    public mutating func didOpen(hello: Hello) -> [Action] {
        state = .awaitingWelcome
        return [.send(.hello(hello))]
    }

    /// Transport closed for any reason.
    public mutating func didClose(reason: String) -> [Action] {
        guard state != .closed else { return [] }
        state = .closed
        return [.emit(.disconnected(reason: reason))]
    }

    /// Heartbeat timer fired.
    public mutating func heartbeatDue() -> [Action] {
        guard case .live(_, let hb) = state else { return [] }
        return [.send(.ping), .scheduleHeartbeat(seconds: hb)]
    }

    /// A frame arrived.
    public mutating func received(_ envelope: Envelope) -> [Action] {
        pruneSeenWakes()
        switch (state, envelope.message) {
        case (.awaitingWelcome, .welcome(let w)):
            state = .live(sessionID: w.sessionID, heartbeatSeconds: w.heartbeatSeconds)
            return [.emit(.connected(w)), .scheduleHeartbeat(seconds: w.heartbeatSeconds)]

        case (.awaitingWelcome, .error(let e)):
            state = .closed
            return [.emit(.protocolError(e)), .close]

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

        case (.live, .error(let e)):
            if e.fatal {
                state = .closed
                return [.emit(.protocolError(e)), .close]
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
