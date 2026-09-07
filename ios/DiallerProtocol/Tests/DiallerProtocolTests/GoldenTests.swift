import XCTest
@testable import DiallerProtocol

/// Proves the Swift side round-trips every golden fixture that the Go server
/// also round-trips (SPEC §5 component 1). Both read protocol/fixtures.
final class GoldenTests: XCTestCase {
    static let fixturesDir: URL = {
        // …/ios/DiallerProtocol/Tests/DiallerProtocolTests/GoldenTests.swift → repo root
        var url = URL(fileURLWithPath: #filePath)
        for _ in 0 ..< 5 { url.deleteLastPathComponent() }
        return url.appendingPathComponent("protocol/fixtures")
    }()

    static let knownTypes: Set<String> = [
        "hello", "welcome", "ping", "pong", "wake", "wake_ack", "wake_cancel", "directory_changed", "error",
    ]

    func testEveryFixtureRoundTrips() throws {
        let files = try FileManager.default.contentsOfDirectory(at: Self.fixturesDir, includingPropertiesForKeys: nil)
            .filter { $0.pathExtension == "json" }
        XCTAssertFalse(files.isEmpty, "no fixtures at \(Self.fixturesDir.path)")

        var seen = Set<String>()
        for file in files {
            let raw = try Data(contentsOf: file)
            let envelope = try WireCoding.decode(raw)
            if case .unknown(let t) = envelope.message {
                XCTFail("\(file.lastPathComponent): type \(t) decoded as unknown")
            }
            seen.insert(envelope.message.typeName)

            // Frame → decode → re-encode must be semantically identical.
            var decoder = FrameDecoder()
            decoder.append(try Frame.encode(envelope))
            let again = try XCTUnwrap(try decoder.nextEnvelope())
            XCTAssertEqual(again, envelope, file.lastPathComponent)

            let reencoded = try WireCoding.encode(again)
            let lhs = try JSONSerialization.jsonObject(with: raw) as! NSDictionary
            let rhs = try JSONSerialization.jsonObject(with: reencoded) as! NSDictionary
            XCTAssertEqual(lhs, rhs, "\(file.lastPathComponent) drifted:\n\(String(decoding: reencoded, as: UTF8.self))")
        }
        XCTAssertEqual(seen, Self.knownTypes, "every v1 type needs a fixture and vice versa")
    }

    func testRejectsBadEnvelopes() {
        let ts = "\"2026-09-05T10:00:00Z\""
        XCTAssertThrowsError(try WireCoding.decode(Data("{\"v\":2,\"type\":\"ping\",\"id\":\"x\",\"ts\":\(ts)}".utf8))) {
            XCTAssertEqual($0 as? WireError, .unsupportedVersion(2))
        }
        XCTAssertThrowsError(try WireCoding.decode(Data("{\"v\":1,\"type\":\"ping\",\"id\":\"\",\"ts\":\(ts)}".utf8))) {
            XCTAssertEqual($0 as? WireError, .missingID)
        }
        // Unknown type is not an error.
        let e = try? WireCoding.decode(Data("{\"v\":1,\"type\":\"future\",\"id\":\"x\",\"ts\":\(ts),\"body\":{\"a\":1}}".utf8))
        XCTAssertEqual(e?.message, .unknown(type: "future"))
    }

    func testFramingSplitsAndCoalesces() throws {
        let a = try Frame.encode(Data("aaa".utf8))
        let b = try Frame.encode(Data("bb".utf8))
        var d = FrameDecoder()
        // Deliver 1.5 frames, then the rest.
        d.append(a + b.prefix(3))
        XCTAssertEqual(try d.next(), Data("aaa".utf8))
        XCTAssertNil(try d.next())
        d.append(b.suffix(from: 3))
        XCTAssertEqual(try d.next(), Data("bb".utf8))
        XCTAssertNil(try d.next())
    }

    func testFrameLimits() {
        XCTAssertThrowsError(try Frame.encode(Data(count: Frame.maxPayload + 1)))
        var d = FrameDecoder()
        d.append(Data([0xff, 0xff, 0xff, 0xff]))
        XCTAssertThrowsError(try d.next()) {
            if case .frameTooLarge = ($0 as? WireError) {} else { XCTFail("wrong error \($0)") }
        }
    }
}
