import DiallerProtocol
import XCTest
@testable import DiallerCore

final class CallTextTests: XCTestCase {
    func testShortDigitStringsAreExtensions() {
        XCTAssertEqual(CallText.party("118"), "Ext 118")
        XCTAssertEqual(CallText.party("100200"), "Ext 100200")
        XCTAssertEqual(CallText.party("+442079460321"), "+442079460321")
        XCTAssertEqual(CallText.party("07700900123"), "07700900123", "eleven digits is a phone number")
        XCTAssertEqual(CallText.party("dev-a"), "dev-a")
        XCTAssertEqual(CallText.party(""), "")
    }

    func testDurations() {
        XCTAssertEqual(CallText.duration(0), "0s")
        XCTAssertEqual(CallText.duration(48), "48s")
        XCTAssertEqual(CallText.duration(134), "2m 14s")
        XCTAssertEqual(CallText.duration(3780), "1h 3m")
    }

    func testWhenFollowsPhoneRecents() {
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "Europe/London")!
        let gb = Locale(identifier: "en_GB")
        // Sunday 27 September 2026, 15:00 London.
        let now = cal.date(from: DateComponents(year: 2026, month: 9, day: 27, hour: 15))!
        func at(_ d: Int, _ h: Int, _ m: Int = 0) -> Date { cal.date(from: DateComponents(year: 2026, month: 9, day: d, hour: h, minute: m))! }
        XCTAssertEqual(CallText.when(at(27, 14, 5), now: now, calendar: cal, locale: gb), "14:05")
        XCTAssertEqual(CallText.when(at(26, 23), now: now, calendar: cal, locale: gb), "Yesterday")
        XCTAssertEqual(CallText.when(at(21, 8), now: now, calendar: cal, locale: gb), "Monday")
        XCTAssertEqual(CallText.when(at(20, 8), now: now, calendar: cal, locale: gb), "20/09/2026")
    }

    func testInitials() {
        XCTAssertEqual(CallText.initials("Amara Nwosu"), "AN")
        XCTAssertEqual(CallText.initials("Facilities"), "F")
        XCTAssertEqual(CallText.initials("Anne-Marie de la Cruz"), "AC", "first and last")
        XCTAssertEqual(CallText.initials("zoë adams"), "ZA")
        XCTAssertNil(CallText.initials("+44 20 7946 0321"))
        XCTAssertNil(CallText.initials(""))
    }

    func testRowDetail() {
        let t0 = Date(timeIntervalSince1970: 1_000_000)
        func record(_ uri: String, _ outcome: CallRecord.Outcome, talked: TimeInterval? = nil) -> CallRecord {
            CallRecord(id: "c", direction: .incoming, counterpart: Party(uri: uri), startedAt: t0,
                       connectedAt: talked.map { t0.addingTimeInterval(100 - $0) }, endedAt: t0.addingTimeInterval(100), outcome: outcome)
        }
        XCTAssertEqual(CallText.detail(for: record("118@pbx", .completed, talked: 134), name: "Priya Shah"), "Ext 118 · 2m 14s")
        XCTAssertEqual(CallText.detail(for: record("100@pbx", .missed), name: "Reception"), "Ext 100 · Missed")
        XCTAssertEqual(CallText.detail(for: record("+442079460321@pbx", .completed, talked: 302), name: "+442079460321"), "5m 2s",
                       "the number is already the row's name")
    }
}
