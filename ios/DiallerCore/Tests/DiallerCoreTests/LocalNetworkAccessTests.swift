import Network
import XCTest
@testable import DiallerCore

final class LocalNetworkAccessTests: XCTestCase {
    /// How a TCP connection waits while Local Network privacy holds it: a
    /// POSIX error, with the reason on the path (device, 2026-09-28).
    private let tcpWaiting = NWConnection.State.waiting(.posix(.ENETDOWN))
    /// How a Bonjour-style refusal shows: the reason is in the error.
    private let dnsDenied = NWConnection.State.waiting(.dns(LocalNetworkAccess.policyDenied))

    private func decide(_ s: NWConnection.State?, pathDenied: Bool = false, answered: Bool = true,
                        deniedFor: TimeInterval = 0, waitingFor: TimeInterval = 5) -> LocalNetworkAccess.Outcome? {
        LocalNetworkAccess.decide(s, pathDenied: pathDenied, answered: answered, deniedFor: deniedFor, settle: 1.5, waitingFor: waitingFor)
    }

    func testReadyMeansAllowed() {
        XCTAssertEqual(decide(.ready), .allowed)
    }

    /// The bug: a TCP wait whose path says Local Network denied was taken
    /// for "not reached", and enrolment ran ahead of the prompt.
    func testTCPWaitBlockedByThePathWaitsForTheAnswer() {
        XCTAssertNil(decide(tcpWaiting, pathDenied: true, answered: false, deniedFor: 30), "the prompt is on screen")
        XCTAssertNil(decide(tcpWaiting, pathDenied: true, deniedFor: 0.5), "answered, not settled yet")
        XCTAssertEqual(decide(tcpWaiting, pathDenied: true, deniedFor: 1.5), .denied)
    }

    func testDNSPolicyDeniedIsTheSame() {
        XCTAssertNil(decide(dnsDenied, answered: false, deniedFor: 30))
        XCTAssertEqual(decide(dnsDenied, deniedFor: 1.5), .denied)
    }

    /// Any other trouble is the enrolment request's to report, after the
    /// one-second backstop.
    func testOtherFailuresAreNotReached() {
        XCTAssertEqual(decide(.waiting(.posix(.ECONNREFUSED))), .notReached)
        XCTAssertEqual(decide(.failed(.posix(.ETIMEDOUT))), .notReached)
        XCTAssertEqual(decide(.waiting(.dns(-65554))), .notReached, "another DNS error")
    }

    func testAWaitWithNoReasonYetGetsTheBackstop() {
        XCTAssertNil(decide(tcpWaiting, waitingFor: 0.3))
        XCTAssertEqual(decide(tcpWaiting, waitingFor: 1.0), .notReached)
    }

    func testStillStartingKeepsWaiting() {
        XCTAssertNil(decide(nil))
        XCTAssertNil(decide(.preparing))
    }
}
