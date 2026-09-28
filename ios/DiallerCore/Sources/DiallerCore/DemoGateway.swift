import DiallerProtocol
import Foundation

/// The demo's gateway (see `Demo`): welcomes the phone with the demo line as
/// soon as it connects, and nothing else ever arrives. What the controller
/// sends (wake acknowledgements) has no one to go to. Incoming demo calls
/// come from `DemoCallEngine`, the way an INVITE reaches an app in front.
public final class DemoGateway: SignalTransport, @unchecked Sendable {
    public let events: AsyncStream<SignalEvent>
    private let continuation: AsyncStream<SignalEvent>.Continuation
    private let now: () -> Date

    public init(now: @escaping () -> Date = Date.init) {
        (events, continuation) = AsyncStream.makeStream(of: SignalEvent.self)
        self.now = now
    }

    public func connect(hello _: Hello) {
        continuation.yield(.connected(Demo.welcome(now: now())))
    }

    public func send(_: Message) {}

    public func disconnect() {
        continuation.finish()
    }

    public func checkLiveness() {}
}
