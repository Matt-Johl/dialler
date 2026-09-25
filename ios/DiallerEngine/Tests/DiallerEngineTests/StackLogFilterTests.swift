import XCTest
@testable import DiallerEngine

/// Which of the stack's log lines reach the app's own log. The wording
/// filter exists to keep libre's chatter out; it must never be what decides
/// whether a warning is seen.
final class StackLogFilterTests: XCTestCase {
    /// The lines that went missing on 2026-09-25: libre patch level 5's SIP
    /// connection lifecycle, written at warning severity from the "transp"
    /// module. Not a keyword on the list, "TLS" not "tls" — and the whole
    /// point of a capture, dropped.
    func testAWarningAlwaysReachesTheLog() {
        let lines = [
            "transp: Dialler: SIP conn opened: local=10.18.0.204:65309 peer=10.18.0.212:5061 TLS",
            "transp: Dialler: SIP conn closed: local=10.18.0.204:65309 peer=10.18.0.212:5061 TLS (Connection reset by peer)",
            "tcp: recv handler: recv(): Socket is not connected [57]",
            "something no keyword would ever match",
        ]
        for line in lines {
            XCTAssertTrue(BaresipCallEngine.stackLineReachesAppLog(line, warning: true), line)
        }
    }

    /// Ordinary chatter is still filtered by wording, as before.
    func testChatterIsFilteredByWording() {
        XCTAssertTrue(BaresipCallEngine.stackLineReachesAppLog("tls: SSL_write: 5", warning: false))
        XCTAssertTrue(BaresipCallEngine.stackLineReachesAppLog("stream: audio: starting mediaenc", warning: false))
        XCTAssertTrue(BaresipCallEngine.stackLineReachesAppLog("cbaresip: ua_alloc: creating", warning: false))
        XCTAssertFalse(BaresipCallEngine.stackLineReachesAppLog("transp: Dialler: SIP conn opened: local=… TLS", warning: false),
                       "without the severity bit this line is exactly what the filter drops — the bit is what saves it")
        XCTAssertFalse(BaresipCallEngine.stackLineReachesAppLog("ua: incoming OPTIONS message from …", warning: false))
    }
}
