import XCTest
@testable import DiallerCore

final class AddressBookTests: XCTestCase {
    func contact(_ id: String, _ name: String, version: Int64, deleted: Bool? = nil) -> DirectoryContact {
        DirectoryContact(id: id, displayName: name, uri: "sip:\(id)@dialler", mode: "local", version: version, deleted: deleted)
    }

    func testDeltaApplyWithTombstones() {
        var book = AddressBook()
        book.apply(DirectorySync(version: 2, since: 0, contacts: [contact("a", "Zed", version: 1), contact("b", "Amy", version: 2)]))
        XCTAssertEqual(book.version, 2)
        XCTAssertEqual(book.sorted.map(\.displayName), ["Amy", "Zed"])

        book.apply(DirectorySync(version: 3, since: 2, contacts: [contact("a", "Zed", version: 3, deleted: true)]))
        XCTAssertEqual(book.version, 3)
        XCTAssertEqual(book.sorted.map(\.id), ["b"])

        // Server JSON shape round-trips (snake_case keys).
        let json = #"{"version":4,"since":3,"contacts":[{"id":"c","display_name":"Desk","uri":"sip:100@asterisk","mode":"trunk","version":4,"updated_at":"2026-09-05T10:00:00Z"}]}"#
        let sync = try! JSONDecoder().decode(DirectorySync.self, from: Data(json.utf8))
        book.apply(sync)
        XCTAssertEqual(book.contacts["c"]?.mode, "trunk")
    }

    func testConfigCompletenessAndHello() {
        var cfg = AppConfig(gateway: GatewayEndpoint(host: "127.0.0.1", acceptAnyCertificate: true), deviceID: "dev-a", token: "")
        XCTAssertFalse(cfg.isComplete)
        cfg.token = "tok"
        XCTAssertTrue(cfg.isComplete)
        let h = cfg.hello(kind: .extensionKind)
        XCTAssertEqual(h.deviceID, "dev-a")
        XCTAssertEqual(h.client, .extensionKind)
        XCTAssertEqual(cfg.httpBase().absoluteString, "http://127.0.0.1:8080")
    }
}
