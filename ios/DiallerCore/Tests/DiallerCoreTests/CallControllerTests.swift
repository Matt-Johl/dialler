import XCTest
import DiallerProtocol
@testable import DiallerCore

final class FakeCallUI: CallUI {
    var reported: [(String, String)] = []
    var ended: [(String, CallEndReason)] = []
    var refuse: Error?
    func reportIncoming(callID: String, displayName: String, handle: String, completion: @escaping (Error?) -> Void) {
        reported.append((callID, displayName))
        completion(refuse)
    }
    func end(callID: String, reason: CallEndReason) { ended.append((callID, reason)) }
}

final class FakeEngine: CallEngine {
    var onIncomingCall: ((String) -> Void)?
    var onCallEnded: ((String) -> Void)?
    var registered: [String] = []
    var prepared: [(String, SIPTarget)] = []
    var users: [String] = []
    var hungUp: [String] = []
    func register(user: String, sip: SIPTarget) { registered.append(user) }
    func prepareForIncomingCall(callID: String, user: String, sip: SIPTarget) { prepared.append((callID, sip)); users.append(user) }
    func hangup(callID: String) { hungUp.append(callID) }
}

final class CallControllerTests: XCTestCase {
    let t0 = Date(timeIntervalSince1970: 1_800_000_000)

    func wake(_ id: String, expiresIn: TimeInterval = 30, name: String? = "Reception") -> Wake {
        Wake(callID: id, from: Party(displayName: name, uri: "sip:100@pbx"), to: Party(uri: "sip:201@dialler"),
             sip: SIPTarget(host: "dialler", port: 5061, transport: "tls"), expiresAt: t0.addingTimeInterval(expiresIn))
    }

    func make() -> (CallController, FakeCallUI, FakeEngine, FakeTransport) {
        let ui = FakeCallUI(), engine = FakeEngine(), tr = FakeTransport()
        let c = CallController(ui: ui, engine: engine, now: { self.t0 })
        c.attach(transport: tr)
        return (c, ui, engine, tr)
    }

    func testWakeRingsThenAcksThenAnswerRegistersSIP() {
        let (c, ui, engine, tr) = make()
        c.handle(.wake(wake("c1")))
        XCTAssertEqual(ui.reported.map { $0.0 }, ["c1"])
        XCTAssertEqual(ui.reported.first?.1, "Reception")
        XCTAssertEqual(tr.sent, [.wakeAck(WakeAck(callID: "c1", action: .willAnswer))])
        XCTAssertEqual(c.activeCalls.map(\.phase), [.ringing])

        c.userAnswered(callID: "c1")
        XCTAssertEqual(engine.prepared.map { $0.0 }, ["c1"])
        XCTAssertEqual(engine.prepared.first?.1.port, 5061)
        XCTAssertEqual(engine.users, ["201@dialler"], "user part of the wake's to URI")
        XCTAssertEqual(CallController.userPart(of: "\"Matt\" <sip:201@dialler;transport=tls>;tag=x"), "201@dialler")
        XCTAssertEqual(CallController.userPart(of: "201"), "201")
        XCTAssertEqual(c.activeCalls.map(\.phase), [.answered])

        c.userEnded(callID: "c1")
        XCTAssertEqual(engine.hungUp, ["c1"])
        XCTAssertTrue(c.activeCalls.isEmpty)
        XCTAssertEqual(tr.sent.count, 1, "no decline ack after an answered call")
    }

    func testDuplicateWakeFromSecondTransportIsIgnored() {
        let (c, ui, _, tr) = make()
        c.handle(wake: wake("c1")) // e.g. via PushKit from the extension
        c.handle(.wake(wake("c1"))) // and again via the foreground socket
        XCTAssertEqual(ui.reported.count, 1)
        XCTAssertEqual(tr.sent.count, 1)
    }

    func testCancelEndsRingingCall() {
        let (c, ui, engine, _) = make()
        c.handle(.wake(wake("c1")))
        c.handle(.wakeCancel(WakeCancel(callID: "c1", reason: .callerHangup)))
        XCTAssertEqual(ui.ended.map { $0.0 }, ["c1"])
        XCTAssertEqual(ui.ended.first?.1, .remoteEnded)
        XCTAssertEqual(engine.hungUp, ["c1"])
        XCTAssertTrue(c.activeCalls.isEmpty)
        c.handle(.wakeCancel(WakeCancel(callID: "ghost", reason: .timeout)))
        XCTAssertEqual(ui.ended.count, 1, "unknown call cancel is ignored")
    }

    func testDeclineWhileRingingSendsDeclineAck() {
        let (c, _, _, tr) = make()
        c.handle(.wake(wake("c1")))
        c.userEnded(callID: "c1")
        XCTAssertEqual(tr.sent.last, .wakeAck(WakeAck(callID: "c1", action: .decline)))
    }

    func testExpiredWakeAndUIRefusal() {
        let (c, ui, _, tr) = make()
        c.handle(.wake(wake("old", expiresIn: -1)))
        XCTAssertTrue(ui.reported.isEmpty)
        XCTAssertTrue(c.activeCalls.isEmpty)

        ui.refuse = NSError(domain: "callkit", code: 1)
        c.handle(.wake(wake("c2")))
        XCTAssertEqual(tr.sent.last, .wakeAck(WakeAck(callID: "c2", action: .busy)))
        XCTAssertTrue(c.activeCalls.isEmpty)
    }

    func testRegisteredAppRingsFromInviteAlone() {
        // Foreground path: welcome carried the SIP account, the app registered,
        // and the server delivers the INVITE directly (no wake, or wake late).
        let (c, ui, engine, tr) = make()
        c.setAccount(user: "201@dialler", sip: SIPTarget(host: "10.0.0.1", port: 5061, transport: "tls"))
        XCTAssertEqual(engine.registered, ["201@dialler"])

        engine.onIncomingCall?("sip:202@dialler")
        XCTAssertEqual(ui.reported.count, 1)
        XCTAssertEqual(ui.reported.first?.1, "sip:202@dialler")
        XCTAssertTrue(tr.sent.isEmpty, "no wake → nothing to ack")
        let id = ui.reported.first!.0

        // A late wake for the same call must not ring a second time.
        c.handle(.wake(wake("late")))
        XCTAssertEqual(ui.reported.count, 1)

        c.userAnswered(callID: id)
        XCTAssertEqual(engine.prepared.map { $0.0 }, [id])

        engine.onCallEnded?("BYE")
        XCTAssertEqual(ui.ended.map { $0.0 }, [id])
        XCTAssertTrue(c.activeCalls.isEmpty)
    }

    func testWakeThenInviteIsOneCall() {
        let (c, ui, engine, _) = make()
        c.setAccount(user: "201@dialler", sip: SIPTarget(host: "10.0.0.1", port: 5061, transport: "tls"))
        c.handle(.wake(wake("c1")))
        engine.onIncomingCall?("sip:100@pbx")
        XCTAssertEqual(ui.reported.count, 1, "wake + INVITE for one call ring once")
        XCTAssertEqual(c.activeCalls.first?.sipArrived, true)
        engine.onCallEnded?("caller hung up")
        XCTAssertEqual(ui.ended.map { $0.0 }, ["c1"])
    }

    func testDisplayNameFallsBackToURI() {
        let (c, ui, _, _) = make()
        c.handle(.wake(wake("c3", name: nil)))
        XCTAssertEqual(ui.reported.first?.1, "sip:100@pbx")
    }
}
