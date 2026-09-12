import XCTest
import DiallerProtocol
@testable import DiallerCore

/// Records the backoff delays the session asks for, without waiting.
final class SleepLog: @unchecked Sendable {
    private let lock = NSLock()
    private var _delays: [TimeInterval] = []
    var delays: [TimeInterval] { lock.withLock { _delays } }
    func sleep(_ d: TimeInterval) async throws { lock.withLock { _delays.append(d) } }
}

final class GatewaySessionTests: XCTestCase {
    let welcome = Welcome(sessionID: "s1", heartbeatSeconds: 25, serverTime: Date(), directoryVersion: 0)
    let hello = AppConfig(gateway: GatewayEndpoint(host: "h"), deviceID: "dev-a", token: "tok").hello(kind: .app)

    func make() -> (GatewaySession, FakeTransport, SleepLog) {
        let inner = FakeTransport(), sleeps = SleepLog()
        let s = GatewaySession(transport: inner, sleep: { try await sleeps.sleep($0) })
        return (s, inner, sleeps)
    }

    /// Poll for an asynchronous effect (the forwarding pump and the
    /// reconnect task are Tasks).
    func eventually(_ what: String, _ cond: () -> Bool, file: StaticString = #filePath, line: UInt = #line) {
        let deadline = Date().addingTimeInterval(2)
        while Date() < deadline {
            if cond() { return }
            RunLoop.current.run(until: Date().addingTimeInterval(0.01))
        }
        XCTFail("timed out waiting for \(what)", file: file, line: line)
    }

    func settle() { RunLoop.current.run(until: Date().addingTimeInterval(0.15)) }

    func testReconnectsAfterADropWithBackoffThatResetsOnSuccess() {
        let (s, inner, sleeps) = make()
        s.connect(hello: hello)
        XCTAssertEqual(inner.connectCount, 1)
        inner.deliver(.connected(welcome))
        settle()

        inner.deliver(.disconnected(reason: "read: ECONNABORTED"))
        eventually("first reconnect") { inner.connectCount == 2 }
        XCTAssertEqual(sleeps.delays, [1], "first retry after 1 s")

        // Still down: the next drop backs off further.
        inner.deliver(.disconnected(reason: "failed"))
        eventually("second reconnect") { inner.connectCount == 3 }
        XCTAssertEqual(sleeps.delays, [1, 2])

        // Up again, then dropped: backoff starts over.
        inner.deliver(.connected(welcome))
        settle()
        inner.deliver(.disconnected(reason: "read"))
        eventually("reconnect after a good session") { inner.connectCount == 4 }
        XCTAssertEqual(sleeps.delays, [1, 2, 1])
    }

    func testNoReconnectAfterTheUserDisconnected() {
        let (s, inner, sleeps) = make()
        s.connect(hello: hello)
        inner.deliver(.connected(welcome))
        settle()
        s.disconnect()
        XCTAssertEqual(inner.disconnectCount, 1)
        inner.deliver(.disconnected(reason: "cancelled"))
        settle()
        XCTAssertEqual(inner.connectCount, 1, "a deliberate disconnect must stay disconnected")
        XCTAssertTrue(sleeps.delays.isEmpty)
    }

    func testBackgroundPausesRetriesAndForegroundReconnectsAtOnce() {
        let (s, inner, sleeps) = make()
        s.connect(hello: hello)
        inner.deliver(.connected(welcome))
        settle()

        s.setActive(false) // app went to the background
        inner.deliver(.disconnected(reason: "read: ECONNABORTED"))
        settle()
        XCTAssertEqual(inner.connectCount, 1, "no retries while suspended")
        XCTAssertTrue(sleeps.delays.isEmpty)

        s.setActive(true) // back to the foreground
        eventually("immediate reconnect on foreground") { inner.connectCount == 2 }
        XCTAssertTrue(sleeps.delays.isEmpty, "foreground reconnect does not wait")
        XCTAssertEqual(inner.lastHello, hello, "reconnects with the same hello")
    }

    func testAnAttemptStuckInWaitingIsAbandonedAndRetried() {
        // A reconnect that hits the server mid-restart: the transport parks
        // in "waiting" (TLS closed / refused) and would never leave it on
        // its own. After the grace it is torn down and the next backoff runs.
        let (s, inner, sleeps) = make()
        s.connect(hello: hello)
        inner.deliver(.connected(welcome))
        settle()
        inner.deliver(.disconnected(reason: "server closed"))
        eventually("reconnect attempt") { inner.connectCount == 2 }

        inner.deliver(.waiting(reason: "-9816: server closed session with no notification"))
        eventually("stuck attempt torn down") { inner.disconnectCount == 1 }
        XCTAssertEqual(sleeps.delays, [1, s.waitingGrace], "backoff, then the waiting grace")

        // The transport reports the teardown as a drop; that schedules the
        // next, longer backoff.
        inner.deliver(.disconnected(reason: "client disconnect"))
        eventually("next attempt") { inner.connectCount == 3 }
        XCTAssertEqual(sleeps.delays, [1, s.waitingGrace, 2])

        // Once an attempt succeeds, a pending grace is dropped.
        inner.deliver(.connected(welcome))
        settle()
        XCTAssertEqual(inner.disconnectCount, 1)
    }

    func testForegroundWhileConnectedDoesNothing() {
        let (s, inner, _) = make()
        s.connect(hello: hello)
        inner.deliver(.connected(welcome))
        settle()
        s.setActive(true)
        settle()
        XCTAssertEqual(inner.connectCount, 1)
    }

    func testEventsPassThroughWithAReconnectNotice() {
        let (s, inner, _) = make()
        let collected = Collected()
        let task = Task { for await ev in s.events { collected.add(ev) } }
        defer { task.cancel() }
        s.connect(hello: hello)
        inner.deliver(.connected(welcome))
        inner.deliver(.disconnected(reason: "read: ECONNABORTED"))
        eventually("events forwarded") { collected.events.count >= 3 }
        XCTAssertEqual(collected.events[0], .connected(welcome))
        XCTAssertEqual(collected.events[1], .disconnected(reason: "read: ECONNABORTED"))
        XCTAssertEqual(collected.events[2], .waiting(reason: "reconnecting in 1s"), "the app can show why it is waiting")
    }
}

final class Collected: @unchecked Sendable {
    private let lock = NSLock()
    private var _events: [SignalEvent] = []
    var events: [SignalEvent] { lock.withLock { _events } }
    func add(_ e: SignalEvent) { lock.withLock { _events.append(e) } }
}

final class ReconnectPolicyTests: XCTestCase {
    func testBackoffCapsAndResets() {
        var p = ReconnectPolicy(delays: [1, 2, 4])
        XCTAssertEqual([p.nextDelay(), p.nextDelay(), p.nextDelay(), p.nextDelay()], [1, 2, 4, 4])
        p.reset()
        XCTAssertEqual(p.nextDelay(), 1)
    }
}
