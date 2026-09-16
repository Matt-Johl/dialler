import XCTest
@testable import DiallerCore

final class CallProgressTests: XCTestCase {
    /// The screen said "Calling…" while congestion played in the earpiece
    /// (2026-09-15). Every refusal now has words of its own, and they agree
    /// with the tone: whatever plays a tone says something.
    func testEveryRefusalIsDescribed() {
        let cases: [(Int, CallProgress)] = [
            (486, .busy), (600, .busy), (603, .declined), (408, .noAnswer),
            (404, .unknownNumber), (410, .unknownNumber),
            (480, .unavailable), (503, .unavailable),
            (500, .failed), (606, .failed),
        ]
        for (status, want) in cases {
            XCTAssertEqual(CallProgress.forFailure(status: status), want, "status \(status)")
        }
    }

    /// The two must not disagree: anything that plays a failure tone needs
    /// text to go with it, or the earpiece contradicts the screen.
    func testAnythingWithAToneHasText() {
        for status in [404, 408, 410, 480, 486, 500, 503, 600, 603, 606] {
            let tone = CallTones.failure(status: status)
            let text = CallProgress.forFailure(status: status)
            XCTAssertNotNil(tone, "status \(status): no tone")
            XCTAssertNotNil(text, "status \(status): a tone plays but the screen says nothing")
        }
    }

    /// And silence needs no explanation either way.
    func testNormalEndingsAreSilentAndUnlabelled() {
        for status in [0, 200, 487] {
            XCTAssertNil(CallTones.failure(status: status), "status \(status)")
            XCTAssertNil(CallProgress.forFailure(status: status), "status \(status)")
        }
    }

    /// A transfer failure carries its status as text, not a number:
    /// baresip passes the far end's NOTIFY sipfrag through verbatim.
    func testTransferFailureReasonIsUnderstood() {
        XCTAssertEqual(CallProgress.forTransferFailure(reason: "486 Busy Here"), .busy)
        XCTAssertEqual(CallProgress.forTransferFailure(reason: "603 Decline"), .declined)
        XCTAssertEqual(CallProgress.forTransferFailure(reason: "480 Temporarily Unavailable"), .unavailable)
        XCTAssertEqual(CallProgress.forTransferFailure(reason: "404 Not Found"), .unknownNumber)
    }

    /// A transfer that failed locally has no SIP status at all — baresip
    /// formats an errno instead. Better to say "Transfer failed" than to
    /// invent a reason from a number that is not one.
    func testTransferFailureWithoutAStatusIsNotGuessedAt() {
        for reason in ["Connection timed out", "", "transfer failed", "48 short", "4860 long"] {
            XCTAssertNil(CallProgress.forTransferFailure(reason: reason), "\(reason)")
        }
    }

    func testOnlyFailuresEndTheCall() {
        XCTAssertFalse(CallProgress.calling.isFailure)
        XCTAssertFalse(CallProgress.ringing.isFailure)
        for p: CallProgress in [.busy, .declined, .noAnswer, .unavailable, .unknownNumber, .failed] {
            XCTAssertTrue(p.isFailure, "\(p)")
        }
    }

    func testLabelsAreShortAndDistinct() {
        let all: [CallProgress] = [.calling, .ringing, .busy, .declined, .noAnswer, .unavailable, .unknownNumber, .failed]
        XCTAssertEqual(Set(all.map(\.label)).count, all.count, "two states share a label")
        for p in all {
            XCTAssertFalse(p.label.isEmpty, "\(p)")
            XCTAssertLessThanOrEqual(p.label.count, 16, "\(p): too long for the in-call screen")
        }
    }
}
