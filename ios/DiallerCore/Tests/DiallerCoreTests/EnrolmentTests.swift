import XCTest
@testable import DiallerCore

/// The enrolment link, the claim, and the pin (SPEC §4.8, §6 item 8).
final class EnrolmentTests: XCTestCase {
    func testLinkParsesTheQRAndNormalisesTheCode() {
        let url = URL(string: "dialler://enrol?c=a7k2-m9px&f=c2hh_-&h=10.18.0.212&p=8080")!
        let link = EnrolmentLink(url: url)
        XCTAssertEqual(link, EnrolmentLink(host: "10.18.0.212", port: 8080, code: "A7K2M9PX", certSHA256: "c2hh_-"))
        XCTAssertEqual(link?.claimURL.absoluteString, "https://10.18.0.212:8080/v1/enrol")

        // Port defaults; a missing fingerprint is nil, not "".
        XCTAssertEqual(EnrolmentLink(url: URL(string: "dialler://enrol?h=srv&c=ABCD1234&f=")!),
                       EnrolmentLink(host: "srv", port: 8080, code: "ABCD1234", certSHA256: nil))
        // Not ours.
        XCTAssertNil(EnrolmentLink(url: URL(string: "https://example.com/enrol?h=x&c=y")!))
        XCTAssertNil(EnrolmentLink(url: URL(string: "dialler://other?h=x&c=y")!))
        XCTAssertNil(EnrolmentLink(url: URL(string: "dialler://enrol?h=x")!), "no code")
        XCTAssertNil(EnrolmentLink(url: URL(string: "dialler://enrol?c=x")!), "no host")

        XCTAssertEqual(EnrolmentLink.normalise("oil0 - abcd"), "0110ABCD")
    }

    func testResultBecomesAPinnedConfig() throws {
        let json = #"{"device_id":"dev_A7K2M9","user":"201","token":"tok_x","signal_port":7443,"sip_domain":"dialler","cert_sha256":"FP"}"#
        let r = try JSONDecoder().decode(EnrolmentResult.self, from: Data(json.utf8))
        let cfg = r.config(host: "10.18.0.212")
        XCTAssertEqual(cfg.deviceID, "dev_A7K2M9")
        XCTAssertEqual(cfg.token, "tok_x")
        XCTAssertEqual(cfg.gateway.host, "10.18.0.212")
        XCTAssertEqual(cfg.gateway.port, 7443)
        XCTAssertEqual(cfg.gateway.certSHA256, "FP")
        XCTAssertFalse(cfg.gateway.acceptAnyCertificate, "enrolled: pinned, never accept-any")
        XCTAssertTrue(cfg.isComplete)
    }

    func testClaimRequestAndAnswers() async throws {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        let client = EnrolmentClient(session: URLSession(configuration: config))
        let link = EnrolmentLink(host: "srv", port: 8080, code: "A7K2M9PX", certSHA256: "FP")

        StubURLProtocol.respond = { _ in (200, #"{"device_id":"d","user":"201","token":"tok_new","signal_port":7443,"sip_domain":"dialler","cert_sha256":"FP"}"#) }
        let r = try await client.claim(link)
        XCTAssertEqual(r.token, "tok_new")
        let req = StubURLProtocol.last!
        XCTAssertEqual(req.httpMethod, "POST")
        XCTAssertEqual(req.url?.absoluteString, "https://srv:8080/v1/enrol")
        XCTAssertNil(req.value(forHTTPHeaderField: "Authorization"), "the claim is the one unauthenticated request")
        let body = try JSONSerialization.jsonObject(with: StubURLProtocol.lastBody!) as! [String: String]
        XCTAssertEqual(body, ["code": "A7K2M9PX"])

        StubURLProtocol.respond = { _ in (404, "unknown or expired enrolment code") }
        await assertThrows(EnrolmentError.badCode) { try await client.claim(link) }
        StubURLProtocol.respond = { _ in (429, "too many attempts") }
        await assertThrows(EnrolmentError.tooManyAttempts) { try await client.claim(link) }
        StubURLProtocol.respond = { _ in (500, "boom") }
        await assertThrows(EnrolmentError.server("HTTP 500")) { try await client.claim(link) }
        StubURLProtocol.respond = { _ in (200, "not json") }
        await assertThrows(EnrolmentError.server("unreadable reply")) { try await client.claim(link) }
    }

    func testFingerprintIsBase64URLOfSHA256() {
        // SHA-256("abc") is the classic vector; base64url drops the padding.
        XCTAssertEqual(CertificatePin.fingerprint(der: Data("abc".utf8)), "ungWv48Bz-pBQUDeXa4iI7ADYaOWF3qctBD_YfIAFa0")
    }

    private func assertThrows(_ want: EnrolmentError, _ op: () async throws -> Void, file: StaticString = #filePath, line: UInt = #line) async {
        do {
            try await op()
            XCTFail("expected \(want)", file: file, line: line)
        } catch let e as EnrolmentError {
            XCTAssertEqual(e, want, file: file, line: line)
        } catch {
            XCTFail("unexpected \(error)", file: file, line: line)
        }
    }
}
