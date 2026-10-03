import CryptoKit
import XCTest
@testable import DiallerCore

/// The phone's half of the licence (SPEC §4.9): the vendor's signature and
/// the date, checked with a key pair of the test's own.
final class LicenceVerifierTests: XCTestCase {
    let now = Date(timeIntervalSince1970: 1_800_000_000) // 2027-01-15
    let priv = Curve25519.Signing.PrivateKey()
    var key: Data { priv.publicKey.rawRepresentation }

    func b64url(_ d: Data) -> String {
        d.base64EncodedString().replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "")
    }

    /// Sign a payload the way the vendor tool does.
    func sign(_ payload: String, with k: Curve25519.Signing.PrivateKey? = nil) throws -> String {
        let p = b64url(Data(payload.utf8))
        let sig = try (k ?? priv).signature(for: Data(("DL1." + p).utf8))
        return "DL1." + p + "." + b64url(sig)
    }

    let good = #"{"v":1,"id":"lic_X","customer":"Example Ltd","install_id":"abc","seats":10,"issued_at":"2026-10-03T12:00:00Z","valid_until":"2027-10-03T12:00:00Z"}"#

    func testAGenuineUnexpiredLicenceIsValid() throws {
        let v = LicenceVerifier.verify(try sign(good), now: now, key: key)
        XCTAssertEqual(v, .valid(validUntil: LicenceVerifier.parseDate("2027-10-03T12:00:00Z")!))
        XCTAssertNil(v.reason)
    }

    func testExpiryIsJudgedByThePhonesClock() throws {
        let tok = try sign(good)
        let until = LicenceVerifier.parseDate("2027-10-03T12:00:00Z")!
        XCTAssertEqual(LicenceVerifier.verify(tok, now: until.addingTimeInterval(-1), key: key), .valid(validUntil: until))
        XCTAssertEqual(LicenceVerifier.verify(tok, now: until, key: key), .expired)
        XCTAssertEqual(LicenceVerifier.verify(tok, now: until.addingTimeInterval(86_400), key: key), .expired)
        XCTAssertEqual(LicenceVerdict.expired.reason, "licence expired")
    }

    func testMissingAndTamperedAndForeignTokensAreRefused() throws {
        XCTAssertEqual(LicenceVerifier.verify(nil, now: now, key: key), .missing)
        XCTAssertEqual(LicenceVerifier.verify("", now: now, key: key), .missing)
        let tok = try sign(good)
        let parts = tok.split(separator: ".").map(String.init)
        XCTAssertEqual(LicenceVerifier.verify("DL2." + parts[1] + "." + parts[2], now: now, key: key), .invalid, "prefix")
        XCTAssertEqual(LicenceVerifier.verify("DL1." + parts[1], now: now, key: key), .invalid, "two parts")
        XCTAssertEqual(LicenceVerifier.verify(tok + ".x", now: now, key: key), .invalid, "four parts")
        XCTAssertEqual(LicenceVerifier.verify("DL1.!!!." + parts[2], now: now, key: key), .invalid, "bad base64")
        // Edit the expiry inside the signed payload.
        let edited = b64url(Data(good.replacingOccurrences(of: "2027-10-03", with: "2037-10-03").utf8))
        XCTAssertEqual(LicenceVerifier.verify("DL1." + edited + "." + parts[2], now: now, key: key), .invalid, "edited payload")
        // Signed by someone else.
        XCTAssertEqual(LicenceVerifier.verify(try sign(good, with: Curve25519.Signing.PrivateKey()), now: now, key: key), .invalid, "another key")
        // Signed, but not a licence this build understands.
        XCTAssertEqual(LicenceVerifier.verify(try sign(#"{"v":2,"valid_until":"2027-10-03T12:00:00Z"}"#), now: now, key: key), .invalid, "v2")
        XCTAssertEqual(LicenceVerifier.verify(try sign(#"{"v":1}"#), now: now, key: key), .invalid, "no date")
        XCTAssertEqual(LicenceVerifier.verify(try sign("not json"), now: now, key: key), .invalid, "not json")
        XCTAssertEqual(LicenceVerifier.verify("DL1." + String(repeating: "A", count: 5000) + "." + parts[2], now: now, key: key), .invalid, "too long")
    }

    func testTheCompiledInKeyIsTheVendors() {
        XCTAssertEqual(LicenceVerifier.vendorPublicKey.count, 32)
        // A token signed with a key of our own does not verify against it.
        XCTAssertEqual(LicenceVerifier.verify(try? sign(good), now: now), .invalid)
    }

    func testDatesWithFractionalSecondsParse() {
        XCTAssertNotNil(LicenceVerifier.parseDate("2027-10-03T12:00:00.5Z"))
        XCTAssertNotNil(LicenceVerifier.parseDate("2027-10-03T12:00:00Z"))
        XCTAssertNil(LicenceVerifier.parseDate("2027-10-03"))
    }
}
