import Network
import XCTest
@testable import DiallerCore

final class LocalNetworkAccessTests: XCTestCase {
    private let denied = NWConnection.State.waiting(.dns(LocalNetworkAccess.policyDenied))

    func testReadyMeansAllowed() {
        XCTAssertEqual(LocalNetworkAccess.decide(.ready, answered: true, deniedFor: 0, settle: 1.5), .allowed)
    }

    /// The prompt is on screen: denial is not final while the user decides,
    /// however long they take.
    func testPolicyDeniedWaitsWhileTheUserDecides() {
        XCTAssertNil(LocalNetworkAccess.decide(denied, answered: false, deniedFor: 30, settle: 1.5))
    }

    /// Answered (Don't Allow, or refused on an earlier run): final once it
    /// has stayed denied for the settle time, not before.
    func testPolicyDeniedIsFinalOnceSettled() {
        XCTAssertNil(LocalNetworkAccess.decide(denied, answered: true, deniedFor: 0.5, settle: 1.5))
        XCTAssertEqual(LocalNetworkAccess.decide(denied, answered: true, deniedFor: 1.5, settle: 1.5), .denied)
    }

    /// Any other trouble is the enrolment request's to report.
    func testOtherFailuresAreNotReached() {
        XCTAssertEqual(LocalNetworkAccess.decide(.waiting(.posix(.ECONNREFUSED)), answered: true, deniedFor: 0, settle: 1.5), .notReached)
        XCTAssertEqual(LocalNetworkAccess.decide(.failed(.posix(.ETIMEDOUT)), answered: true, deniedFor: 0, settle: 1.5), .notReached)
        XCTAssertEqual(LocalNetworkAccess.decide(.waiting(.dns(-65554)), answered: true, deniedFor: 9, settle: 1.5), .notReached, "another DNS error")
    }

    func testStillStartingKeepsWaiting() {
        XCTAssertNil(LocalNetworkAccess.decide(nil, answered: true, deniedFor: 0, settle: 1.5))
        XCTAssertNil(LocalNetworkAccess.decide(.preparing, answered: true, deniedFor: 0, settle: 1.5))
    }
}
