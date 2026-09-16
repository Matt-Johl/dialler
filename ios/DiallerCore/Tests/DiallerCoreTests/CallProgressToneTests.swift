import XCTest
import DiallerProtocol
@testable import DiallerCore

/// The tone plan as the controller applies it (plan Phase J): which tone,
/// when, and what it does to the end of a failed call. The tones themselves
/// (cadence, PCM) are `CallTonesTests`.
final class CallProgressToneTests: XCTestCase {
    let sip = SIPTarget(host: "dialler", port: 5061, transport: "tls")

    /// A controller whose deferred ends fire when the test says so, not
    /// seconds later.
    func make() -> (CallController, FakeCallUI, FakeEngine, () -> Void) {
        let ui = FakeCallUI(), engine = FakeEngine()
        let c = CallController(ui: ui, engine: engine)
        var pending: [() -> Void] = []
        c.schedule = { _, work in pending.append(work) }
        c.setAccount(user: "201@dialler", sip: sip)
        return (c, ui, engine, { pending.forEach { $0() }; pending = [] })
    }

    /// Dial 202 and let the system UI approve it. Returns the call's id —
    /// outgoing and incoming calls share one counter, so it is not always
    /// "out-1".
    @discardableResult
    func dial(_ c: CallController) -> String {
        let id = c.startCall(to: "202")!
        c.userStarted(callID: id)
        return id
    }

    // MARK: ring-back

    func testRingbackWhileTheFarEndRings() {
        let (c, ui, engine, _) = make()
        dial(c)
        XCTAssertTrue(ui.tones.isEmpty, "nothing until the far end answers the INVITE")
        engine.onOutgoingRinging?("e-out-1", false)
        XCTAssertEqual(ui.tones, [CallTones.ringback])
        engine.onCallEstablished?("e-out-1")
        XCTAssertEqual(ui.tones, [CallTones.ringback, nil], "answered: the ring-back stops")
    }

    /// 183 with SDP: the far end's own ring-back or announcement is already
    /// in the caller's ear, so ours must not be laid over it.
    func testEarlyMediaSuppressesRingback() {
        let (c, ui, engine, _) = make()
        dial(c)
        engine.onOutgoingRinging?("e-out-1", true)
        XCTAssertTrue(ui.tones.isEmpty, "nothing started, so nothing to stop either")
        XCTAssertEqual(ui.connecting, ["out-1"], "the call is still reported as connecting")
    }

    /// The usual order from a PBX is 180 then 183: the tone starts and then
    /// gives way to the early media.
    func testEarlyMediaAfterRingingStopsTheTone() {
        let (c, ui, engine, _) = make()
        dial(c)
        engine.onOutgoingRinging?("e-out-1", false)
        engine.onOutgoingRinging?("e-out-1", true)
        XCTAssertEqual(ui.tones, [CallTones.ringback, nil])
    }

    /// `startCall` refuses a second call, so ring-back can only ever start
    /// on its own. It can still be *running* when a second call arrives and
    /// is answered (call waiting) — the ear is a conversation's from then on.
    func testAnsweringAnotherCallStopsRingback() {
        let (c, ui, engine, _) = make()
        dial(c)
        engine.onOutgoingRinging?("e-out-1", false)
        XCTAssertEqual(ui.tones, [CallTones.ringback])
        c.handle(sipIncoming: "sip:203@dialler", engineCallID: "e2")
        c.userAnswered(callID: "sip-2")
        XCTAssertEqual(ui.tones.last!, nil)
    }

    /// And the outgoing call refused behind that conversation gets no
    /// failure tone: it would play into the ear of someone talking.
    func testNoFailureToneOverAnAnsweredCall() {
        let (c, ui, engine, _) = make()
        dial(c)
        engine.onOutgoingRinging?("e-out-1", false)
        c.handle(sipIncoming: "sip:203@dialler", engineCallID: "e2")
        c.userAnswered(callID: "sip-2")
        engine.onCallEnded?("e-out-1", "486 Busy Here", 486)
        XCTAssertEqual(ui.tones.last!, nil)
        XCTAssertEqual(ui.ended.map { $0.0 }, ["out-1"], "and it is reported ended at once")
    }

    /// The app has one player and the call-waiting beep shares it, so a
    /// tone is only ever stopped by the call that started it: the end of a
    /// call that never had one leaves the ring-back alone.
    func testAnotherCallsEndDoesNotStopTheRingback() {
        let (c, ui, engine, _) = make()
        dial(c)
        engine.onOutgoingRinging?("e-out-1", false)
        c.handle(sipIncoming: "sip:203@dialler", engineCallID: "e2")
        engine.onCallEnded?("e2", "Connection reset by peer", 0)
        XCTAssertEqual(ui.tones, [CallTones.ringback, nil], "stopped once, by the arriving call, not twice")
    }

    // MARK: what the screen says

    /// "Calling…" is only true until the far end is reached.
    func testRingingIsReportedWhenTheFarEndAlerts() {
        let (c, ui, engine, _) = make()
        dial(c)
        XCTAssertTrue(ui.progress.isEmpty, "nothing to say yet")
        engine.onOutgoingRinging?("e-out-1", false)
        XCTAssertEqual(ui.progress.map(\.1), [.ringing])
    }

    /// The bug this exists for: the screen read "Calling…" while the
    /// earpiece played congestion. The reason must arrive BEFORE the end,
    /// because the call is still on screen for as long as its tone lasts.
    func testRefusalIsExplainedBeforeTheCallDisappears() {
        for (status, want) in [(486, CallProgress.busy), (603, .declined), (480, .unavailable), (404, .unknownNumber)] {
            let (c, ui, engine, fire) = make()
            dial(c)
            engine.onCallEnded?("e-out-1", "refused", status)
            XCTAssertEqual(ui.progress.map(\.1), [want], "status \(status)")
            XCTAssertTrue(ui.ended.isEmpty, "status \(status): explained while still on screen")
            fire()
            XCTAssertEqual(ui.ended.map { $0.0 }, ["out-1"], "status \(status)")
        }
    }

    /// A call that simply ended needs no explanation; the screen goes.
    func testNoExplanationForANormalClear() {
        let (c, ui, engine, _) = make()
        dial(c)
        engine.onOutgoingRinging?("e-out-1", false)
        engine.onCallEnded?("e-out-1", "Connection reset by peer", 0)
        XCTAssertEqual(ui.progress.map(\.1), [.ringing], "ringing only; nothing about the ending")
    }

    // MARK: failure tones

    /// Busy: the tone plays first and the system UI is told the call ended
    /// only when it is done — CallKit takes the audio session away with the
    /// call, so a tone reported after the end would be cut off.
    func testBusyToneIsHeardBeforeTheCallDisappears() {
        let (c, ui, engine, fire) = make()
        dial(c)
        engine.onOutgoingRinging?("e-out-1", false)
        engine.onCallEnded?("e-out-1", "486 Busy Here", 486)
        XCTAssertEqual(ui.tones, [CallTones.ringback, nil, CallTones.busy])
        XCTAssertTrue(ui.ended.isEmpty, "the end waits for the tone")
        XCTAssertTrue(c.activeCalls.isEmpty, "the call itself is gone either way")

        fire()
        XCTAssertEqual(ui.tones.last!, nil)
        XCTAssertEqual(ui.ended.map { $0.0 }, ["out-1"])
        XCTAssertEqual(ui.ended.map { $0.1 }, [.failed])
    }

    /// Anything else that refused the call: 480 is what our own server
    /// returns when no connection can be woken.
    func testCongestionToneForOtherFailures() {
        let (c, ui, engine, fire) = make()
        dial(c)
        engine.onCallEnded?("e-out-1", "480 Temporarily Unavailable", 480)
        XCTAssertEqual(ui.tones, [CallTones.congestion])
        fire()
        XCTAssertEqual(ui.ended.map { $0.1 }, [.failed])
    }

    /// A call that simply ended, and our own CANCEL, are silent.
    func testNoToneForANormalClearOrOurOwnCancel() {
        for (reason, status) in [("Connection reset by peer", 0), ("487 Request Terminated", 487)] {
            let (c, ui, engine, _) = make()
            dial(c)
            engine.onCallEnded?("e-out-1", reason, status)
            XCTAssertTrue(ui.tones.isEmpty, "\(reason): the player is never touched")
            XCTAssertEqual(ui.ended.map { $0.1 }, [.remoteEnded], "\(reason): reported at once")
        }
    }

    /// An incoming call that fails gets no tone: nobody dialled it, and the
    /// person it failed for is not holding the phone to their ear.
    func testNoFailureToneOnAnIncomingCall() {
        let (c, ui, engine, _) = make()
        c.handle(sipIncoming: "sip:202@dialler", engineCallID: "e1")
        engine.onCallEnded?("e1", "486 Busy Here", 486)
        XCTAssertTrue(ui.tones.isEmpty)
        XCTAssertEqual(ui.ended.map { $0.0 }, ["sip-1"])
    }

    /// Hanging up on the busy tone ends the call there and then.
    func testHangingUpOnTheToneEndsTheCallAtOnce() {
        let (c, ui, engine, fire) = make()
        dial(c)
        engine.onCallEnded?("e-out-1", "486 Busy Here", 486)
        c.userEnded(callID: "out-1")
        XCTAssertEqual(ui.ended.map { $0.0 }, ["out-1"])
        XCTAssertEqual(ui.tones.last!, nil, "the tone stops with it")
        fire()
        XCTAssertEqual(ui.ended.count, 1, "the tone's own deadline does not report it twice")
    }

    /// A call arriving while the tone plays owns the ear from then on.
    func testAnIncomingCallStopsAFailureTone() {
        let (c, ui, engine, fire) = make()
        dial(c)
        engine.onCallEnded?("e-out-1", "503 Service Unavailable", 503)
        XCTAssertEqual(ui.tones.last!, CallTones.congestion)
        c.handle(sipIncoming: "sip:202@dialler", engineCallID: "e1")
        XCTAssertEqual(ui.tones.last!, nil)
        XCTAssertEqual(ui.ended.map { $0.0 }, ["out-1"], "the failed call is reported ended, not left behind")
        XCTAssertEqual(ui.reported.map { $0.0 }, ["sip-2"], "the two directions share one call counter")
        fire()
        XCTAssertEqual(ui.ended.count, 1)
    }
}
