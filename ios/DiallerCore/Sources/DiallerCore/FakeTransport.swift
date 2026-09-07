import DiallerProtocol
import Foundation

/// In-memory transport for tests and previews: records what the app sends
/// and lets the test push events as if the gateway had spoken.
public final class FakeTransport: SignalTransport {
    public let events: AsyncStream<SignalEvent>
    private let continuation: AsyncStream<SignalEvent>.Continuation
    private let lock = NSLock()
    private var _sent: [Message] = []
    private var _hello: Hello?
    private var _disconnects = 0

    public init() {
        var c: AsyncStream<SignalEvent>.Continuation!
        events = AsyncStream { c = $0 }
        continuation = c
    }

    public var sent: [Message] { lock.withLock { _sent } }
    public var lastHello: Hello? { lock.withLock { _hello } }
    public var disconnectCount: Int { lock.withLock { _disconnects } }

    public func connect(hello: Hello) { lock.withLock { _hello = hello } }
    public func send(_ message: Message) { lock.withLock { _sent.append(message) } }
    public func disconnect() { lock.withLock { _disconnects += 1 } }

    /// Test hook: deliver an event to the app.
    public func deliver(_ event: SignalEvent) { continuation.yield(event) }
    public func finish() { continuation.finish() }
}
