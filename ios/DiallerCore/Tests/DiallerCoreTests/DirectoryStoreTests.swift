import XCTest
@testable import DiallerCore

/// The persisted address book, the search, and the client's write requests
/// (SPEC §6 item 7).
final class DirectoryStoreTests: XCTestCase {
    func contact(_ id: String, _ name: String, uri: String, favourite: Bool? = nil) -> DirectoryContact {
        DirectoryContact(id: id, displayName: name, uri: uri, mode: "local", version: 1, favourite: favourite)
    }

    func testBookRoundTripsThroughTheStore() {
        let url = FileManager.default.temporaryDirectory.appendingPathComponent("book-\(UUID().uuidString).json")
        addTeardownBlock { try? FileManager.default.removeItem(at: url) }
        let store = AddressBookStore(url: url)
        XCTAssertEqual(store.load(), AddressBook(), "nothing saved yet: an empty book at version 0")

        var book = AddressBook()
        book.apply(DirectorySync(version: 5, since: 0, contacts: [
            contact("a", "Zed", uri: "sip:1@x"),
            contact("b", "Amy", uri: "sip:2@x", favourite: true),
        ]))
        store.save(book)
        let loaded = store.load()
        XCTAssertEqual(loaded, book)
        XCTAssertEqual(loaded.version, 5, "the cursor survives: the next sync is a delta, not a full one")
        XCTAssertEqual(loaded.favourites.map(\.id), ["b"])
        XCTAssertEqual(loaded.sorted.map(\.displayName), ["Amy", "Zed"])

        // The server omits favourite when false; the book treats nil as false.
        let json = #"{"version":6,"since":5,"contacts":[{"id":"c","display_name":"Desk","uri":"sip:100@asterisk","mode":"trunk","version":6}]}"#
        book.apply(try! JSONDecoder().decode(DirectorySync.self, from: Data(json.utf8)))
        XCTAssertFalse(book.contacts["c"]!.isFavourite)
        book.setFavourite(id: "c", true)
        XCTAssertEqual(book.favourites.map(\.id), ["b", "c"])
        book.reset()
        XCTAssertEqual(book, AddressBook())
        store.clear()
        XCTAssertEqual(store.load(), AddressBook())
    }

    func testSearchMatchesNameOrExtensionIgnoringCaseAndAccents() {
        let list = [
            contact("1", "Zoë Reception", uri: "sip:100@asterisk"),
            contact("2", "Matt (201)", uri: "sip:201@dialler"),
            contact("3", "Warehouse", uri: "sip:2015@dialler"),
        ]
        XCTAssertEqual(DirectorySearch.filter(list, query: "").map(\.id), ["1", "2", "3"], "blank matches all")
        XCTAssertEqual(DirectorySearch.filter(list, query: "  ").map(\.id), ["1", "2", "3"])
        XCTAssertEqual(DirectorySearch.filter(list, query: "zoe").map(\.id), ["1"], "diacritic-insensitive")
        XCTAssertEqual(DirectorySearch.filter(list, query: "RECEP").map(\.id), ["1"], "case-insensitive substring")
        XCTAssertEqual(DirectorySearch.filter(list, query: "201").map(\.id), ["2", "3"], "any substring of the extension")
        XCTAssertEqual(DirectorySearch.filter(list, query: "2015").map(\.id), ["3"])
        XCTAssertEqual(DirectorySearch.filter(list, query: "asterisk").map(\.id), [], "the host is not searched")
        XCTAssertEqual(DirectorySearch.filter(list, query: "nobody").map(\.id), [])
    }

    func testWriteRequestsCarryDeviceAuthAndTheDraft() async throws {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        let session = URLSession(configuration: config)
        let client = DirectoryClient(base: URL(string: "https://server:8080")!, deviceID: "dev-a", token: "tok", session: session)

        StubURLProtocol.respond = { req in
            (200, #"{"id":"ct_1","display_name":"Desk","uri":"sip:100@asterisk","mode":"trunk","version":9,"favourite":true}"#)
        }
        let created = try await client.create(ContactDraft(displayName: "Desk", uri: "sip:100@asterisk", mode: "trunk", favourite: true))
        XCTAssertEqual(created.id, "ct_1")
        XCTAssertEqual(created.version, 9)
        XCTAssertTrue(created.isFavourite)
        var req = StubURLProtocol.last!
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.url?.path, "/v1/directory")
        XCTAssertEqual(req.value(forHTTPHeaderField: "X-Device-ID"), "dev-a")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer tok")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Content-Type"), "application/json")
        let body = try JSONSerialization.jsonObject(with: StubURLProtocol.lastBody!) as! [String: Any]
        XCTAssertEqual(body["display_name"] as? String, "Desk")
        XCTAssertEqual(body["uri"] as? String, "sip:100@asterisk")
        XCTAssertEqual(body["mode"] as? String, "trunk")
        XCTAssertEqual(body["favourite"] as? Bool, true)

        _ = try await client.update(id: "ct_1", ContactDraft(displayName: "Desk!", uri: "sip:100@asterisk", mode: "trunk"))
        req = StubURLProtocol.last!
        XCTAssertEqual(req.httpMethod, "PUT")
        XCTAssertEqual(req.url?.path, "/v1/directory/ct_1")

        StubURLProtocol.respond = { _ in (204, "") }
        try await client.delete(id: "ct_1")
        req = StubURLProtocol.last!
        XCTAssertEqual(req.httpMethod, "DELETE")
        XCTAssertEqual(req.url?.path, "/v1/directory/ct_1")

        StubURLProtocol.respond = { _ in (404, "not found") }
        do {
            try await client.delete(id: "ct_9")
            XCTFail("a 404 must throw")
        } catch {}
    }
}

/// Answers every request of a session with a canned status and body, and
/// keeps the last request (and its body) for inspection.
final class StubURLProtocol: URLProtocol {
    nonisolated(unsafe) static var respond: (URLRequest) -> (Int, String) = { _ in (200, "{}") }
    nonisolated(unsafe) static var last: URLRequest?
    nonisolated(unsafe) static var lastBody: Data?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.last = request
        // URLSession moves httpBody into a stream before the protocol sees it.
        if let stream = request.httpBodyStream {
            stream.open()
            var data = Data()
            var buf = [UInt8](repeating: 0, count: 4096)
            while stream.hasBytesAvailable {
                let n = stream.read(&buf, maxLength: buf.count)
                if n <= 0 { break }
                data.append(buf, count: n)
            }
            stream.close()
            Self.lastBody = data
        } else {
            Self.lastBody = request.httpBody
        }
        let (status, body) = Self.respond(request)
        let resp = HTTPURLResponse(url: request.url!, statusCode: status, httpVersion: "HTTP/1.1", headerFields: ["Content-Type": "application/json"])!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(body.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}
