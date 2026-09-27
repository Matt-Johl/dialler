@testable import DiallerCore
import XCTest

/// The bridge's answer to a CallKit provider reset and to a start refused
/// as "unknown call provider", pinned to the device history of 2026-09-27.
final class ProviderRecoveryTests: XCTestCase {
    /// 2026-09-14 14:47, 2026-09-15 08:04 and 10:44: reset, active within
    /// 200 ms, outgoing accepted. Registering afresh is still the answer;
    /// nothing more is owed once the app is active.
    func testResetWhileActiveRebuildsOnce() {
        var r = ProviderRecovery()
        XCTAssertEqual(r.didReset(appActive: true), [.rebuild("provider reset")])
        XCTAssertFalse(r.resetTakenInBackground)
        XCTAssertEqual(r.didBecomeActive(), [])
    }

    /// 2026-09-27 10:41:54: reset in the background, active 2.8 s later,
    /// every outgoing call refused until relaunch. Rebuild at the reset and
    /// again on the foreground that follows it, then stop.
    func testResetInBackgroundRebuildsAgainOnForeground() {
        var r = ProviderRecovery()
        XCTAssertEqual(r.didReset(appActive: false), [.rebuild("provider reset, taken in the background")])
        XCTAssertTrue(r.resetTakenInBackground)
        XCTAssertEqual(r.didBecomeActive(), [.rebuild("app active after a reset taken in the background")])
        XCTAssertEqual(r.didBecomeActive(), [], "one foreground rebuild per background reset")
    }

    /// 2026-09-26 13:11:35: reset in the background and an incoming call
    /// reported 1 ms later, accepted. The provider has answered, so the
    /// foreground owes nothing.
    func testProviderAnsweringClearsTheDebt() {
        var r = ProviderRecovery()
        _ = r.didReset(appActive: false)
        r.providerAnswered()
        XCTAssertEqual(r.didBecomeActive(), [])
    }

    /// out-12 on 2026-09-27: refused with code 2 after 10 s. Rebuild and
    /// retry that call once; a second refusal of the same call is final.
    func testUnknownCallProviderRebuildsAndRetriesOnce() {
        var r = ProviderRecovery()
        XCTAssertEqual(
            r.startFailed(callID: "out-12", code: ProviderRecovery.unknownCallProviderCode),
            [.rebuild("start request refused: the system knows no call provider for this app"), .retryStart("out-12")]
        )
        XCTAssertEqual(r.startFailed(callID: "out-12", code: ProviderRecovery.unknownCallProviderCode), [.giveUp("out-12")])
        XCTAssertFalse(r.retried.contains("out-12"), "a given-up call leaves no state behind")
    }

    /// The retry budget is per call: the next call gets its own.
    func testRetryBudgetIsPerCall() {
        var r = ProviderRecovery()
        _ = r.startFailed(callID: "out-12", code: 2)
        _ = r.startFailed(callID: "out-12", code: 2)
        XCTAssertEqual(r.startFailed(callID: "out-13", code: 2).count, 2)
    }

    /// out-16 on 2026-09-27: code 7 (maximum call groups) while an incoming
    /// call was ringing. Not a registration problem; no rebuild.
    func testOtherRefusalsAreFinal() {
        var r = ProviderRecovery()
        XCTAssertEqual(r.startFailed(callID: "out-16", code: 7), [.giveUp("out-16")])
        XCTAssertEqual(r.startFailed(callID: "out-17", code: -1), [.giveUp("out-17")])
    }

    /// A start that went through drops its retry mark.
    func testForgetClearsRetry() {
        var r = ProviderRecovery()
        _ = r.startFailed(callID: "out-12", code: 2)
        r.forget("out-12")
        XCTAssertEqual(r.startFailed(callID: "out-12", code: 2).count, 2, "retry available again")
    }
}

/// "Is another call up?" for the call-waiting decision, both directions.
final class CallRosterTests: XCTestCase {
    /// 2026-09-27 10:43:31: out-16's start had sat unanswered in the system
    /// for 8 s when 101 called. It was not a call; the incoming one must
    /// ring as the only call, not as call waiting.
    func testPendingOutgoingStartIsNotUp() {
        var r = CallRoster()
        r.add("out-16", .awaitingStart)
        XCTAssertFalse(r.anyUp)
        XCTAssertTrue(r.contains("out-16"))
    }

    /// 2026-09-17: an incoming report refused late by a Focus filter.
    func testPendingIncomingReportIsNotUp() {
        var r = CallRoster()
        r.add("sip-1", .awaitingReport)
        XCTAssertFalse(r.anyUp)
    }

    func testConfirmedCallsAreUp() {
        var r = CallRoster()
        r.add("sip-1", .awaitingReport)
        r.confirm("sip-1")
        XCTAssertTrue(r.anyUp)
        r.add("out-2", .awaitingStart)
        r.confirm("out-2")
        XCTAssertEqual(r.state(of: "out-2"), .up)
        r.remove("sip-1")
        XCTAssertTrue(r.anyUp)
        r.remove("out-2")
        XCTAssertFalse(r.anyUp)
        XCTAssertTrue(r.isEmpty)
    }

    func testConfirmingAnUnknownCallAddsNothing() {
        var r = CallRoster()
        r.confirm("ghost")
        XCTAssertTrue(r.isEmpty)
    }

    func testResetClearsEverything() {
        var r = CallRoster()
        r.add("a", .up)
        r.add("b", .awaitingStart)
        r.removeAll()
        XCTAssertTrue(r.isEmpty)
        XCTAssertEqual(r.ids, [])
    }
}
