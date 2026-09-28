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
/// `NWConnection` — raises the prompt and waits in `.waiting` with
/// kDNSServiceErr_PolicyDenied until the answer; allowed, it becomes ready.
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

    /// What the probe concludes from where the connection stands. While it
    /// is policy-denied the user may still be reading the prompt, so denial
    /// is only final once they are not deciding (`answered`: no system
    /// alert over the app) and it has stayed so for `settle` seconds.
    static func decide(_ state: NWConnection.State?, answered: Bool, deniedFor: TimeInterval, settle: TimeInterval) -> Outcome? {
        switch state {
        case .ready?: return .allowed
        case .failed?, .cancelled?: return .notReached
        case .waiting(let error)?:
            guard isPolicyDenied(error) else { return .notReached }
            return answered && deniedFor >= settle ? .denied : nil
        default: return nil // setup, preparing: keep waiting
        }
    }

    /// Open a TCP connection to `host:port` and wait for Local Network
    /// privacy to let it through or not. `answered` says whether the app is
    /// in front with no system alert over it (UIApplication active).
    public static func probe(host: String, port: UInt16, timeout: TimeInterval = 60, settle: TimeInterval = 1.5,
                             answered: @escaping @Sendable () async -> Bool) async -> Outcome {
        guard let nwPort = NWEndpoint.Port(rawValue: port) else { return .notReached }
        let connection = NWConnection(host: NWEndpoint.Host(host), port: nwPort, using: .tcp)
        let state = StateBox()
        connection.stateUpdateHandler = { state.set($0) }
        connection.start(queue: DispatchQueue(label: "local-network-probe"))
        defer { connection.cancel() }

        let start = Date()
        var deniedSince: Date?
        while Date().timeIntervalSince(start) < timeout {
            let now = state.get()
            let isAnswered = await answered()
            if case .waiting(let error)? = now, isPolicyDenied(error), isAnswered {
                deniedSince = deniedSince ?? Date()
            } else {
                deniedSince = nil
            }
            let deniedFor = deniedSince.map { Date().timeIntervalSince($0) } ?? 0
            if let outcome = decide(now, answered: isAnswered, deniedFor: deniedFor, settle: settle) { return outcome }
            try? await Task.sleep(nanoseconds: 200_000_000)
        }
        if case .waiting(let error)? = state.get(), isPolicyDenied(error) { return .denied }
        return .notReached
    }

    private final class StateBox: @unchecked Sendable {
        private let lock = NSLock()
        private var state: NWConnection.State?
        func set(_ s: NWConnection.State) { lock.withLock { state = s } }
        func get() -> NWConnection.State? { lock.withLock { state } }
    }
}
