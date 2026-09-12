import DiallerProtocol
import Foundation

/// How long to wait before each successive reconnect attempt after a drop;
/// reset once a session is up again.
public struct ReconnectPolicy: Equatable, Sendable {
    public var delays: [TimeInterval]
    public private(set) var attempt = 0

    public init(delays: [TimeInterval] = [1, 2, 4, 8, 15]) {
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
            reconnect?.cancel()
            reconnect = nil
        }
        inner.connect(hello: hello)
    }

    public func send(_ message: Message) { inner.send(message) }

    public func disconnect() {
        lock.withLock {
            wanted = false
            reconnect?.cancel()
            reconnect = nil
            waitingGraceTask?.cancel()
            waitingGraceTask = nil
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
            policy.reset()
            return hello
        }
        if let now { inner.connect(hello: now) }
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
            }
        case .disconnected:
            let delay: TimeInterval? = lock.withLock {
                connected = false
                waitingGraceTask?.cancel()
                waitingGraceTask = nil
                guard wanted, active else { return nil }
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
        continuation.yield(.waiting(reason: "reconnecting in \(Int(delay))s"))
        let t = Task { [weak self] in
            guard let self else { return }
            try? await self.sleep(delay)
            if Task.isCancelled { return }
            let hello: Hello? = self.lock.withLock {
                guard self.wanted, self.active, !self.connected else { return nil }
                return self.hello
            }
            if let hello { self.inner.connect(hello: hello) }
        }
        lock.withLock { reconnect = t }
    }
}
