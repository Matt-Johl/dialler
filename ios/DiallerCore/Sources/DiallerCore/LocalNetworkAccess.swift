import Foundation
import Network

/// iOS's Local Network permission, asked for before enrolment rather than
/// discovered by it (appstore.md, must-fix 8).
///
/// iOS has no call that asks for the permission: the prompt appears on the
/// first connection to a local address, and that connection does not wait
/// for the answer. The enrolment request itself used to be that connection,
/// so the first enrolment after installing failed and the prompt appeared
/// behind the error (device, 2026-09-28). A plain TCP connection to the
/// server first — Apple's Local Network Privacy FAQ does the same with
/// `NWConnection` — raises the prompt and waits in `.waiting` until the
/// answer; allowed, it becomes ready.
///
/// How the wait shows: for a TCP connection, `.waiting` with a POSIX error
/// (ENETDOWN) and the path's `unsatisfiedReason == .localNetworkDenied`.
/// The first version looked only for kDNSServiceErr_PolicyDenied (how a
/// Bonjour browse is refused), took the POSIX wait for "some other
/// failure" and went on to enrol at once: the first enrolment still failed
/// ahead of the prompt (device, 2026-09-28 evening).
public enum LocalNetworkAccess {
    public enum Outcome: Equatable, Sendable {
        /// The server's port answered: access is allowed (or not needed).
        case allowed
        /// Refused by Local Network privacy, and the user is not deciding.
        case denied
        /// Anything else (refused, no route, timed out): the enrolment
        /// request will fail with its own, better explained, error.
        case notReached
    }

    /// kDNSServiceErr_PolicyDenied: how Network.framework reports a
    /// connection Local Network privacy stops.
    static let policyDenied: DNSServiceErrorType = -65570

    static func isPolicyDenied(_ error: NWError) -> Bool {
        if case .dns(let code) = error { return code == policyDenied }
        return false
    }

    /// Whether Local Network privacy is what holds the connection: the
    /// path says so (TCP), or the error does (a Bonjour-style DNS refusal).
    static func blockedByPrivacy(_ state: NWConnection.State?, pathDenied: Bool) -> Bool {
        guard case .waiting(let error)? = state else { return false }
        return pathDenied || isPolicyDenied(error)
    }

    /// What the probe concludes from where the connection stands. While
    /// privacy holds it the user may still be reading the prompt, so denial
    /// is only final once they are not deciding (`answered`: no system
    /// alert over the app) and it has stayed so for `settle` seconds.
    ///
    /// A wait with no privacy reason is given `grace` seconds before it
    /// counts as "not reached" — a backstop, in case the path's reason
    /// arrives a moment after the state, not the way the prompt is found.
    static func decide(_ state: NWConnection.State?, pathDenied: Bool, answered: Bool, deniedFor: TimeInterval, settle: TimeInterval,
                       waitingFor: TimeInterval = .infinity, grace: TimeInterval = 1) -> Outcome? {
        switch state {
        case .ready?: return .allowed
        case .failed?, .cancelled?: return .notReached
        case .waiting?:
            guard blockedByPrivacy(state, pathDenied: pathDenied) else { return waitingFor >= grace ? .notReached : nil }
            return answered && deniedFor >= settle ? .denied : nil
        default: return nil // setup, preparing: keep waiting
        }
    }

    /// Open a TCP connection to `host:port` and wait for Local Network
    /// privacy to let it through or not. `answered` says whether the app is
    /// in front with no system alert over it (UIApplication active).
    /// `log` gets each state the connection passes through, with the
    /// path's reason, so a device log shows what the probe saw.
    public static func probe(host: String, port: UInt16, timeout: TimeInterval = 60, settle: TimeInterval = 1.5,
                             log: @escaping @Sendable (String) -> Void = { _ in },
                             answered: @escaping @Sendable () async -> Bool) async -> Outcome {
        guard let nwPort = NWEndpoint.Port(rawValue: port) else { return .notReached }
        let connection = NWConnection(host: NWEndpoint.Host(host), port: nwPort, using: .tcp)
        let box = StateBox()
        connection.stateUpdateHandler = { s in
            // The path's reason read with the state, not after it.
            box.set(s, pathDenied: Self.localNetworkDenied(connection.currentPath))
            log("local network probe: \(s), path \(Self.describe(connection.currentPath))")
        }
        connection.pathUpdateHandler = { path in
            box.setPathDenied(Self.localNetworkDenied(path))
            log("local network probe: path \(Self.describe(path))")
        }
        connection.start(queue: DispatchQueue(label: "local-network-probe"))
        defer { connection.cancel() }

        let start = Date()
        var deniedSince: Date?
        var waitingSince: Date?
        while Date().timeIntervalSince(start) < timeout {
            let (now, pathDenied) = box.get()
            let isAnswered = await answered()
            if case .waiting? = now { waitingSince = waitingSince ?? Date() } else { waitingSince = nil }
            if blockedByPrivacy(now, pathDenied: pathDenied), isAnswered {
                deniedSince = deniedSince ?? Date()
            } else {
                deniedSince = nil
            }
            let deniedFor = deniedSince.map { Date().timeIntervalSince($0) } ?? 0
            let waitingFor = waitingSince.map { Date().timeIntervalSince($0) } ?? 0
            if let outcome = decide(now, pathDenied: pathDenied, answered: isAnswered, deniedFor: deniedFor, settle: settle,
                                    waitingFor: waitingFor) { return outcome }
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
        let (last, pathDenied) = box.get()
        return blockedByPrivacy(last, pathDenied: pathDenied) ? .denied : .notReached
    }

    static func localNetworkDenied(_ path: NWPath?) -> Bool {
        guard let path, path.status != .satisfied else { return false }
        return path.unsatisfiedReason == .localNetworkDenied
    }

    private static func describe(_ path: NWPath?) -> String {
        guard let path else { return "none" }
        if path.status == .satisfied { return "satisfied" }
        return "\(path.status), reason \(path.unsatisfiedReason)"
    }

    private final class StateBox: @unchecked Sendable {
        private let lock = NSLock()
        private var state: NWConnection.State?
        private var pathDenied = false
        func set(_ s: NWConnection.State, pathDenied d: Bool) { lock.withLock { state = s; if d { pathDenied = true } } }
        func setPathDenied(_ d: Bool) { lock.withLock { pathDenied = d } }
        func get() -> (NWConnection.State?, Bool) { lock.withLock { (state, pathDenied) } }
    }
}
