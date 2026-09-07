@testable import DiallerCore
import XCTest

/// The CallKit ↔ engine audio contract, scenario by scenario, including the
/// orderings observed on the device.
final class AudioGateTests: XCTestCase {
    func testEngineStartsHeld() {
        var g = AudioGate()
        XCTAssertTrue(g.held)
        XCTAssertEqual(g.engineStarted(), [.hold])
        XCTAssertTrue(g.held)
    }

    func testAnswerTouchesNoAudio() {
        var g = AudioGate()
        _ = g.engineStarted()
        XCTAssertEqual(g.answerAction(callID: "c1"), [.answer("c1")])
        XCTAssertTrue(g.held, "answering must not release audio; only didActivate may")
    }

    /// Foreground answer as seen on the device: answer → activate → established.
    func testActivationBeforeEstablishment() {
        var g = AudioGate()
        _ = g.engineStarted()
        _ = g.answerAction(callID: "c1")
        XCTAssertEqual(g.didActivate(), [.release])
        XCTAssertFalse(g.held)
        XCTAssertEqual(g.callEstablished(), [], "units allocated on a released gate start by themselves")
    }

    /// Slow activation: answer → established → activate.
    func testEstablishmentBeforeActivation() {
        var g = AudioGate()
        _ = g.engineStarted()
        _ = g.answerAction(callID: "c1")
        XCTAssertEqual(g.callEstablished(), [])
        XCTAssertTrue(g.held, "units allocated while held must wait")
        XCTAssertEqual(g.didActivate(), [.release])
    }

    /// Cold launch from the lock screen: activation can precede the answer.
    func testActivationBeforeAnswer() {
        var g = AudioGate()
        _ = g.engineStarted()
        XCTAssertEqual(g.didActivate(), [.release])
        XCTAssertEqual(g.answerAction(callID: "c1"), [.answer("c1")])
        XCTAssertFalse(g.held)
    }

    func testDeactivateHoldsAgain() {
        var g = AudioGate()
        _ = g.engineStarted()
        _ = g.didActivate()
        XCTAssertEqual(g.didDeactivate(), [.hold])
        XCTAssertTrue(g.held)
        XCTAssertFalse(g.sessionActive)
        // Next call: nothing starts until the next activation.
        XCTAssertEqual(g.callEstablished(), [])
        XCTAssertTrue(g.held)
        XCTAssertEqual(g.didActivate(), [.release])
    }

    func testProviderResetHangsUpAndHolds() {
        var g = AudioGate()
        _ = g.engineStarted()
        _ = g.didActivate()
        XCTAssertEqual(g.providerReset(calls: ["c1", "c2"]), [.hangup("c1"), .hangup("c2"), .hold])
        XCTAssertTrue(g.held)
    }
}
