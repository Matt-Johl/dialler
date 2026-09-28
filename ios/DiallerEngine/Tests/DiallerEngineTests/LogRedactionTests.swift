import XCTest
@testable import DiallerEngine

final class LogRedactionTests: XCTestCase {
    /// The account line as baresip reported it on 2026-09-28, token and all.
    func testAccountLineLosesItsPassword() {
        let line = #"engine event: <sip:203@dialler;transport=tls>;auth_user=dev-s;auth_pass=tok_dev_s_harness_fixed;outbound="sip:10.18.0.212:5061;transport=tls";regint=300"#
        let out = BaresipCallEngine.redacted(line)
        XCTAssertFalse(out.contains("tok_dev_s_harness_fixed"))
        XCTAssertTrue(out.contains("auth_pass=•••;outbound="), out)
        XCTAssertTrue(out.contains("auth_user=dev-s"), "the device id stays: it names the phone, it is not a secret")
    }

    func testPasswordAtTheEndOrQuoted() {
        XCTAssertEqual(BaresipCallEngine.redacted("x;auth_pass=secret"), "x;auth_pass=•••")
        XCTAssertEqual(BaresipCallEngine.redacted("<a;auth_pass=secret>"), "<a;auth_pass=•••>")
        XCTAssertEqual(BaresipCallEngine.redacted(#"auth_pass=secret" more"#), #"auth_pass=•••" more"#)
    }

    func testOtherLinesAreUntouched() {
        let line = "engine: registering 203@dialler via 10.18.0.212:5061/tls"
        XCTAssertEqual(BaresipCallEngine.redacted(line), line)
    }

    /// Whatever closure the host sets, lines reach it redacted.
    func testTheLogPropertyRedacts() {
        let engine = BaresipCallEngine(acceptAnyCertificate: true)
        var seen: [String] = []
        engine.log = { seen.append($0) }
        engine.log("a;auth_pass=secret;b")
        XCTAssertEqual(seen, ["a;auth_pass=•••;b"])
    }
}
