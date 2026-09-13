import DiallerProtocol
import Foundation

/// How long to wait before each successive reconnect attempt after a drop;
/// reset once a session is up again. The first attempt is immediate: a
/// drop the platform reported (a better network path, a reset) leaves
/// nothing worth waiting for, and only a failed attempt earns a backoff.
public struct ReconnectPolicy: Equatable, Sendable {
    public var delays: [TimeInterval]
    public private(set) var attempt = 0

    public init(delays: [TimeInterval] = [0, 1, 2, 4, 8, 15]) {
        self.delays = delays
    }

    public mutating func nextDelay() -> TimeInterval {
        let d = delays[min(attempt, delays.count - 1)]
        attempt += 1
        return d
    }

    public mutating func reset() { attempt = 0 }
}

/// Keeps the gateway session up. Wraps a `SignalTransport`, forwards its
/// events unchanged, and after a drop the app did not ask for reconnects
/// with backoff while the host says it is active (foreground); a return to
/// the foreground reconnects at once. `disconnect()` stops everything.
///
/// Why: iOS tears an app's sockets down while it is suspended (screen
/// locked, backgrounded). The next read then fails with ECONNABORTED and,
/// without this, the app stayed "disconnected" until relaunched — and could
/// not place calls either, because the SIP registration had died in the
/// same suspension and only a new welcome re-establishes it.
public final class GatewaySession: SignalTransport, @unchecked Sendable {
    // @unchecked Sendable: every mutable field below is read and written
    // under `lock`; the transport and continuation are immutable. Hosts
    // hand the session to Tasks and queues (the extension's event pump).
    public let events: AsyncStream<SignalEvent>
    private let continuation: AsyncStream<SignalEvent>.Continuation
    private let inner: SignalTransport
    private let sleep: @Sendable (TimeInterval) async throws -> Void
    private let lock = NSLock()
    private var hello: Hello?
    private var wanted = false // connect() called and no disconnect() since
    private var active = true  // host in the foreground
    private var connected = false
    /// The server refused this session for good (bad enrolment, protocol
    /// version, or a newer session superseded it). Retrying every few
    /// seconds would only fill the server log — and for `superseded`, fight
    /// the newer session — so the backoff stops until the host comes to the
    /// foreground again (the user may have re-enrolled) or `connect` is
    /// called afresh.
    private var refused = false
    private var policy: ReconnectPolicy
    private var reconnect: Task<Void, Never>?
    private var waitingGraceTask: Task<Void, Never>?
    private var pump: Task<Void, Never>?

    /// How long a connect attempt may sit in the transport's "waiting" state
    /// before it is abandoned and retried with backoff. Network.framework
    /// only retries a waiting connection when the network path changes, so
    /// an attempt that hit the server mid-restart (TLS closed, connection
    /// refused) would otherwise wait forever.
    public var waitingGrace: TimeInterval = 5
    /// Deadline for a connect attempt that reports nothing at all — no
    /// .connected, no .waiting, no .disconnected (a connection stuck
    /// preparing while the network path flaps, a peer gone mid-handshake).
    /// Such an attempt used to leave the keeper with no timer, and the
    /// extension sat disconnected for hours after the server came back
    /// (2026-09-13 02:00–05:52) while iOS reported it active. Past the
    /// deadline the attempt is torn down, which yields .disconnected and
    /// the next backoff. nil disables it (unit tests drive events by hand).
    public var attemptTimeout: TimeInterval? = 10
    private var attemptTask: Task<Void, Never>?

    public convenience init(endpoint: GatewayEndpoint, policy: ReconnectPolicy = ReconnectPolicy()) {
        self.init(transport: LANSocketTransport(endpoint: endpoint), policy: policy)
    }

    /// `sleep` is injectable so tests run the backoff without waiting.
    public init(transport: SignalTransport, policy: ReconnectPolicy = ReconnectPolicy(),
                sleep: @escaping @Sendable (TimeInterval) async throws -> Void = { try await Task.sleep(nanoseconds: UInt64($0 * 1_000_000_000)) }) {
        var c: AsyncStream<SignalEvent>.Continuation!
        events = AsyncStream { c = $0 }
        continuation = c
        inner = transport
        self.policy = policy
        self.sleep = sleep
        pump = Task { [weak self] in
            for await ev in transport.events {
                guard let self else { return }
                self.forward(ev)
            }
            self?.continuation.finish()
        }
    }

    deinit {
        pump?.cancel()
        reconnect?.cancel()
        waitingGraceTask?.cancel()
    }

    // MARK: SignalTransport

    public func connect(hello: Hello) {
        lock.withLock {
            self.hello = hello
            wanted = true
            refused = false
            reconnect?.cancel()
            reconnect = nil
        }
        attempt(hello)
    }

    /// One connect attempt, with a deadline. Every attempt gets the
    /// `waitingGrace` timer, not only one the transport reports as
    /// "waiting": an attempt that never reports anything (a connection
    /// stuck preparing while the network path flaps, a peer that vanished
    /// mid-handshake) used to leave the keeper with no timer at all, and
    /// the extension sat disconnected for hours after the server came back
    /// (2026-09-13 02:00–05:52) while iOS reported it active. Past the
    /// deadline the attempt is torn down, which yields `.disconnected` and
    /// the next backoff.
    private func attempt(_ hello: Hello) {
        inner.connect(hello: hello)
        guard let timeout = attemptTimeout else { return }
        let t = Task { [weak self] in
            guard let self else { return }
            try? await self.sleep(timeout)
            if Task.isCancelled { return }
            let abandon: Bool = self.lock.withLock {
                self.attemptTask = nil
                return self.wanted && self.active && !self.connected
            }
            if abandon { self.inner.disconnect() } // → .disconnected → backoff
        }
        lock.withLock {
            attemptTask?.cancel()
            attemptTask = t
        }
    }

    /// Fatal error codes after which the keeper stops retrying on its own.
    static let refusals: Set<ErrorCode> = [.unauthorized, .unsupportedVersion, .superseded]

    public func send(_ message: Message) { inner.send(message) }

    public func disconnect() {
        lock.withLock {
            wanted = false
            reconnect?.cancel()
            reconnect = nil
            waitingGraceTask?.cancel()
            waitingGraceTask = nil
            attemptTask?.cancel()
            attemptTask = nil
        }
        inner.disconnect()
    }

    // MARK: Host lifecycle

    /// Foreground (true): if a wanted session is down, reconnect now.
    /// Background (false): stop retrying; iOS suspends the app anyway and
    /// the Local Push extension covers wakes.
    public func setActive(_ isActive: Bool) {
        let now: Hello? = lock.withLock {
            active = isActive
            reconnect?.cancel()
            reconnect = nil
            guard isActive, wanted, !connected else { return nil }
            refused = false
            policy.reset()
            return hello
        }
        if let now { attempt(now) }
    }

    // MARK: Internals

    private func forward(_ ev: SignalEvent) {
        continuation.yield(ev)
        switch ev {
        case .connected:
            lock.withLock {
                connected = true
                policy.reset()
                waitingGraceTask?.cancel()
                waitingGraceTask = nil
                attemptTask?.cancel()
                attemptTask = nil
            }
        case .protocolError(let e):
            if e.fatal, Self.refusals.contains(e.code) {
                lock.withLock { refused = true }
            }
        case .disconnected:
            let delay: TimeInterval? = lock.withLock {
                connected = false
                waitingGraceTask?.cancel()
                waitingGraceTask = nil
                attemptTask?.cancel()
                attemptTask = nil
                guard wanted, active, !refused else { return nil }
                return policy.nextDelay()
            }
            if let delay { schedule(after: delay) }
        case .waiting(let reason):
            // Our own reconnect notice passes through; a transport "waiting"
            // on an attempt we want gets a bounded grace, then the attempt
            // is torn down so the drop schedules the next backoff.
            guard !reason.hasPrefix("reconnecting") else { return }
            let start: Bool = lock.withLock { wanted && active && !connected && waitingGraceTask == nil }
            if start { startWaitingGrace() }
        default:
            break
        }
    }

    private func startWaitingGrace() {
        let t = Task { [weak self] in
            guard let self else { return }
            try? await self.sleep(self.waitingGrace)
            if Task.isCancelled { return }
            let abandon: Bool = self.lock.withLock {
                self.waitingGraceTask = nil
                return self.wanted && self.active && !self.connected
            }
            if abandon { self.inner.disconnect() } // → .disconnected → backoff
        }
        lock.withLock { waitingGraceTask = t }
    }

    private func schedule(after delay: TimeInterval) {
        continuation.yield(.waiting(reason: delay > 0 ? "reconnecting in \(Int(delay))s" : "reconnecting now"))
        let t = Task { [weak self] in
            guard let self else { return }
            try? await self.sleep(delay)
            if Task.isCancelled { return }
            let hello: Hello? = self.lock.withLock {
                guard self.wanted, self.active, !self.connected else { return nil }
                return self.hello
            }
            if let hello { self.attempt(hello) }
        }
        lock.withLock { reconnect = t }
    }
}
