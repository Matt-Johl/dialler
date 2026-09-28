import CryptoKit
import XCTest
@testable import DiallerCore

final class CertificatePinDigestTests: XCTestCase {
    /// The SIP engine gets the pin as bytes: the round trip from a
    /// certificate's fingerprint must give back exactly its SHA-256.
    func testDigestRoundTripsAFingerprint() {
        let der = Data((0..<600).map { UInt8($0 % 251) })
        let pin = CertificatePin.fingerprint(der: der)
        XCTAssertEqual(CertificatePin.digest(of: pin), Data(SHA256.hash(data: der)))
    }

    /// base64url with the characters base64 spells differently.
    func testDigestReadsURLSafeCharacters() {
        let bytes = Data(repeating: 0xFB, count: 32) // "+/" territory in plain base64
        let pin = bytes.base64URLEncodedString()
        XCTAssertTrue(pin.contains("-") || pin.contains("_"))
        XCTAssertEqual(CertificatePin.digest(of: pin), bytes)
    }

    func testDigestRefusesWhatIsNotASHA256() {
        XCTAssertNil(CertificatePin.digest(of: ""))
        XCTAssertNil(CertificatePin.digest(of: "not base64!"))
        XCTAssertNil(CertificatePin.digest(of: Data(repeating: 1, count: 20).base64URLEncodedString()), "SHA-1 length")
    }
}
