import XCTest
@testable import DiallerCore

final class SSIDListTests: XCTestCase {
    func testSplitsTrimsAndDropsEmptiesAndRepeats() {
        XCTAssertEqual(SSIDList.parse("Office, Office-5G ,,office, Office"), ["Office", "Office-5G", "office"],
                       "case is significant; the repeat is dropped; empties vanish")
    }

    func testBlankInputIsEmpty() {
        XCTAssertEqual(SSIDList.parse(""), [])
        XCTAssertEqual(SSIDList.parse(" , ,\n"), [])
    }

    func testInternalSpacesSurvive() {
        XCTAssertEqual(SSIDList.parse("QPS 10010 5G,  Guest Net "), ["QPS 10010 5G", "Guest Net"])
    }

    func testFormatRoundTrips() {
        let text = "QPS_10010_5G, QPS_10010_2G"
        XCTAssertEqual(SSIDList.format(SSIDList.parse(text)), text)
        XCTAssertEqual(SSIDList.parse(SSIDList.format(["a", "b c"])), ["a", "b c"])
        XCTAssertEqual(SSIDList.format([]), "")
    }
}
