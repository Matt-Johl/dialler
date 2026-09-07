import XCTest
import DiallerProtocol
@testable import DiallerCore

final class FakeCallUI: CallUI {
    var reported: [(String, String)] = []
    var ended: [(String, CallEndReason)] = []
    var refuse: Error?
    var started: [(String, String, String)] = []
    var connecting: [String] = []
    var connected: [String] = []
    func reportIncoming(callID: String, displayName: String, handle: String, completion: @escaping (Error?) -> Void) {
        reported.append((callID, displayName))
        completion(refuse)
    }
    func end(callID: String, reason: CallEndReason) { ended.append((callID, reason)) }
    func startOutgoing(callID: String, handle: String, displayName: String) { started.append((callID, handle, displayName)) }
    func outgoingConnecting(callID: String) { connecting.append(callID) }
    func outgoingConnected(callID: String) { connected.append(callID) }
}

final class FakeEngine: CallEngine {
    var onIncomingCall: ((String) -> Void)?
    var onCallEnded: ((String) -> Void)?
    var onOutgoingRinging: (() -> Void)?
    var onCallEstablished: (() -> Void)?
    var onTransferFailed: ((String) -> Void)?
    var transferred: [(String, String)] = []
    var registered: [String] = []
    var prepared: [(String, SIPTarget)] = []
    var users: [String] = []
    var hungUp: [String] = []
    var dialled: [(String, String)] = []
    var muted: [Bool] = []
    var held: [Bool] = []
    func register(user: String, sip: SIPTarget) { registered.append(user) }
    func prepareForIncomingCall(callID: String, user: String, sip: SIPTarget) { prepared.append((callID, sip)); users.append(user) }
    func dial(callID: String, to target: String) { dialled.append((callID, target)) }
    func hangup(callID: String) { hungUp.append(callID) }
    func setMuted(_ m: Bool) { muted.append(m) }
    func setHeld(_ h: Bool) { held.append(h) }
    func transfer(callID: String, to target: String) { transferred.append((callID, target)) }
}

final class TransferTests: XCTestCase {
    let sip = SIPTarget(host: "dialler", port: 5061, transport: "tls")

    func testTransferOnlyOnAnsweredCalls() {
        let ui = FakeCallUI(), engine = FakeEngine()
        let c = CallController(ui: ui, engine: engine)
        c.setAccount(user: "201@dialler", sip: sip)
        c.handle(sipIncoming: "sip:202@dialler")
        c.transfer(callID: "sip-1", to: "100")
        XCTAssertTrue(engine.transferred.isEmpty, "ringing: nothing to transfer yet")
        c.userAnswered(callID: "sip-1")
        c.transfer(callID: "sip-1", to: " 100 ")
        XCTAssertEqual(engine.transferred.map { $0.1 }, ["100"])
        c.transfer(callID: "sip-1", to: "   ")
        XCTAssertEqual(engine.transferred.count, 1, "empty target ignored")
    }

    func testRefusedTransferIsReportedAndCallContinues() {
        let ui = FakeCallUI(), engine = FakeEngine()
        let c = CallController(ui: ui, engine: engine)
        c.setAccount(user: "201@dialler", sip: sip)
        c.handle(sipIncoming: "sip:202@dialler")
        c.userAnswered(callID: "sip-1")
        var reported: [String] = []
        c.onTransferFailed = { reported.append($0) }
        c.transfer(callID: "sip-1", to: "999")
        engine.onTransferFailed?("404 Not Found")
        XCTAssertEqual(reported, ["404 Not Found"])
        XCTAssertEqual(c.activeCalls.count, 1, "the call is still up after a refused transfer")
        // Success: the far end (server) ends our call once the target answered.
        engine.onCallEnded?("Call transfered")
        XCTAssertTrue(c.activeCalls.isEmpty)
        XCTAssertEqual(ui.ended.map { $0.0 }, ["sip-1"])
    }
}

final class HoldTests: XCTestCase {
    let sip = SIPTarget(host: "dialler", port: 5061, transport: "tls")

    func testHoldAndResumeOnAnsweredCall() {
        let ui = FakeCallUI(), engine = FakeEngine()
        let c = CallController(ui: ui, engine: engine)
        c.setAccount(user: "201@dialler", sip: sip)
        c.startCall(to: "202")
        c.userStarted(callID: "out-1")
        // Not yet answered: a hold makes no sense and must not reach the engine.
        c.setHeld(callID: "out-1", true)
        XCTAssertTrue(engine.held.isEmpty)
        engine.onCallEstablished?()
        c.setHeld(callID: "out-1", true)
        c.setHeld(callID: "out-1", false)
        XCTAssertEqual(engine.held, [true, false])
        // Unknown call ids are ignored.
        c.setHeld(callID: "nope", true)
        XCTAssertEqual(engine.held, [true, false])
    }

    func testHoldOnAnsweredIncomingCall() {
        let ui = FakeCallUI(), engine = FakeEngine()
        let c = CallController(ui: ui, engine: engine)
        c.setAccount(user: "201@dialler", sip: sip)
        c.handle(sipIncoming: "sip:202@dialler")
        c.userAnswered(callID: "sip-1")
        c.setHeld(callID: "sip-1", true)
        XCTAssertEqual(engine.held, [true])
    }
}

final class OutboundCallTests: XCTestCase {
    let sip = SIPTarget(host: "dialler", port: 5061, transport: "tls")

    func make() -> (CallController, FakeCallUI, FakeEngine) {
        let ui = FakeCallUI(), engine = FakeEngine()
        let c = CallController(ui: ui, engine: engine)
        c.setAccount(user: "201@dialler", sip: sip)
        return (c, ui, engine)
    }

    /// Keypad digits or a directory URI: system UI approves, then the engine
    /// dials; progress and answer are reported back to the system UI.
    func testStartCallGoesThroughSystemUIThenDials() {
        let (c, ui, engine) = make()
        let id = c.startCall(to: "202", displayName: "Phone B")
        XCTAssertEqual(id, "out-1")
        XCTAssertEqual(ui.started.map { $0.1 }, ["202"])
        XCTAssertEqual(ui.started.map { $0.2 }, ["Phone B"])
        XCTAssertTrue(engine.dialled.isEmpty, "must not dial before the system approves the call")

        c.userStarted(callID: "out-1")
        XCTAssertEqual(engine.dialled.map { $0.1 }, ["202"])

        engine.onOutgoingRinging?()
        XCTAssertEqual(ui.connecting, ["out-1"])
        engine.onCallEstablished?()
        XCTAssertEqual(ui.connected, ["out-1"])
        XCTAssertEqual(c.activeCalls.first?.phase, .answered)
        XCTAssertEqual(c.activeCalls.first?.direction, .outgoing)

        engine.onCallEnded?("BYE")
        XCTAssertEqual(ui.ended.map { $0.0 }, ["out-1"])
        XCTAssertTrue(c.activeCalls.isEmpty)
    }

    func testUserHangsUpOutgoingCall() {
        let (c, _, engine) = make()
        c.startCall(to: "sip:202@dialler")
        c.userStarted(callID: "out-1")
        c.userEnded(callID: "out-1")
        XCTAssertEqual(engine.hungUp, ["out-1"])
        XCTAssertTrue(c.activeCalls.isEmpty)
    }

    func testNoAccountOrBusyRefusesToDial() {
        let ui = FakeCallUI(), engine = FakeEngine()
        let c = CallController(ui: ui, engine: engine)
        XCTAssertNil(c.startCall(to: "202"), "no SIP account yet")
        c.setAccount(user: "201@dialler", sip: sip)
        XCTAssertNotNil(c.startCall(to: "202"))
        XCTAssertNil(c.startCall(to: "203"), "one call at a time")
        XCTAssertNil(c.startCall(to: "   "))
    }

    func testSystemRefusalDropsTheCall() {
        let (c, _, engine) = make()
        c.startCall(to: "202")
        c.startFailed(callID: "out-1")
        XCTAssertTrue(c.activeCalls.isEmpty)
        XCTAssertTrue(engine.dialled.isEmpty)
    }

    func testMuteReachesEngine() {
        let (c, _, engine) = make()
        c.setMuted(true)
        c.setMuted(false)
        XCTAssertEqual(engine.muted, [true, false])
    }
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
