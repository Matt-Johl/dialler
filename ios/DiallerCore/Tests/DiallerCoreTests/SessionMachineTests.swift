import XCTest
import DiallerProtocol
@testable import DiallerCore

final class SessionMachineTests: XCTestCase {
    let t0 = Date(timeIntervalSince1970: 1_800_000_000)
    lazy var welcome = Welcome(sessionID: "s1", heartbeatSeconds: 25, serverTime: t0, directoryVersion: 3)

    func env(_ m: Message, id: String = "x") -> Envelope { Envelope(id: id, ts: t0, message: m) }

    func wake(_ id: String, expiresIn: TimeInterval = 30) -> Wake {
        Wake(callID: id, from: Party(displayName: "Reception", uri: "sip:100@pbx"), to: Party(uri: "sip:201@dialler"),
             sip: SIPTarget(host: "dialler", port: 5061, transport: "tls"), expiresAt: t0.addingTimeInterval(expiresIn))
    }

    func testHandshakeThenHeartbeat() {
        var m = SessionMachine(now: { self.t0 })
        let hello = Hello(deviceID: "d", token: "t", client: .app)
        XCTAssertEqual(m.didOpen(hello: hello), [.send(.hello(hello))])
        XCTAssertEqual(m.state, .awaitingWelcome)

        // Nothing but welcome/error is honoured before welcome.
        XCTAssertEqual(m.received(env(.wake(wake("c1")))), [])

        XCTAssertEqual(m.received(env(.welcome(welcome))), [.emit(.connected(welcome)), .scheduleHeartbeat(seconds: 25)])
        XCTAssertEqual(m.state, .live(sessionID: "s1", heartbeatSeconds: 25))
        XCTAssertEqual(m.heartbeatDue(), [.send(.ping), .scheduleHeartbeat(seconds: 25)])
        XCTAssertEqual(m.received(env(.pong)), [])
        XCTAssertEqual(m.received(env(.ping)), [.send(.pong)])
    }

    func testFatalErrorBeforeWelcomeCloses() {
        var m = SessionMachine(now: { self.t0 })
        _ = m.didOpen(hello: Hello(deviceID: "d", token: "bad", client: .app))
        let e = ProtocolError(code: .unauthorized, message: nil, fatal: true)
        XCTAssertEqual(m.received(env(.error(e))), [.emit(.protocolError(e)), .close])
        XCTAssertEqual(m.state, .closed)
        XCTAssertEqual(m.didClose(reason: "x"), [], "already closed: no duplicate disconnected event")
    }

    func testWakeDedupAndExpiry() {
        var now = t0
        var m = SessionMachine(now: { now })
        _ = m.didOpen(hello: Hello(deviceID: "d", token: "t", client: .extensionKind))
        _ = m.received(env(.welcome(welcome)))

        let w = wake("c1", expiresIn: 30)
        XCTAssertEqual(m.received(env(.wake(w))), [.emit(.wake(w))])
        XCTAssertEqual(m.received(env(.wake(w), id: "replay")), [], "replayed wake must not ring twice")

        now = t0.addingTimeInterval(31)
        let later = Wake(callID: "c1", from: w.from, to: w.to, sip: w.sip, expiresAt: now.addingTimeInterval(30))
        XCTAssertEqual(m.received(Envelope(id: "again", ts: now, message: .wake(later))), [.emit(.wake(later))],
                       "after expiry the id is free again")

        let c = WakeCancel(callID: "c1", reason: .callerHangup)
        XCTAssertEqual(m.received(env(.wakeCancel(c))), [.emit(.wakeCancel(c))])
        XCTAssertEqual(m.received(env(.directoryChanged(DirectoryChanged(version: 9)))), [.emit(.directoryChanged(9))])
    }

    func testWakeExpiryIsRebasedOntoLocalClock() {
        // Server clock is an hour behind ours (Docker VM drift): its wake says
        // "expires at server-now + 30s", which on our clock is already past.
        let serverNow = t0.addingTimeInterval(-3600)
        var m = SessionMachine(now: { self.t0 })
        _ = m.didOpen(hello: Hello(deviceID: "d", token: "t", client: .app))
        _ = m.received(Envelope(id: "w", ts: serverNow, message: .welcome(welcome)))

        let w = Wake(callID: "c1", from: Party(uri: "sip:100@pbx"), to: Party(uri: "sip:201@dialler"),
                     sip: SIPTarget(host: "dialler", port: 5061, transport: "tls"), expiresAt: serverNow.addingTimeInterval(30))
        let actions = m.received(Envelope(id: "x", ts: serverNow, message: .wake(w)))
        guard case .emit(.wake(let got))? = actions.first else { return XCTFail("no wake emitted: \(actions)") }
        XCTAssertEqual(got.expiresAt.timeIntervalSince(t0), 30, accuracy: 0.001, "deadline should be 30s from OUR now")

        // Small differences (latency, normal drift) are left untouched.
        let near = t0.addingTimeInterval(-2)
        let w2 = Wake(callID: "c2", from: w.from, to: w.to, sip: w.sip, expiresAt: near.addingTimeInterval(30))
        guard case .emit(.wake(let got2))? = m.received(Envelope(id: "y", ts: near, message: .wake(w2))).first else { return XCTFail() }
        XCTAssertEqual(got2.expiresAt, w2.expiresAt)
    }

    func testNonFatalErrorKeepsSession() {
        var m = SessionMachine(now: { self.t0 })
        _ = m.didOpen(hello: Hello(deviceID: "d", token: "t", client: .app))
        _ = m.received(env(.welcome(welcome)))
        let e = ProtocolError(code: .unknownCall, message: "no such call", fatal: false)
        XCTAssertEqual(m.received(env(.error(e))), [.emit(.protocolError(e))])
        XCTAssertEqual(m.state, .live(sessionID: "s1", heartbeatSeconds: 25))
        XCTAssertEqual(m.didClose(reason: "server closed"), [.emit(.disconnected(reason: "server closed"))])
    }
}
