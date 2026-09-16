import XCTest
@testable import DiallerCore

/// The rule that a tone is never what activates CallKit's audio session
/// (plan Phase J). Written after shipping the opposite: ring-back played on
/// the 180, 154 ms before `didActivate`, and the AVAudioPlayer activated
/// the session behind CallKit's back — the app lost its audio on every call.
final class TonePolicyTests: XCTestCase {
    /// The order that broke it: the tone is asked for first and the
    /// session is activated second.
    func testToneAskedForBeforeActivationIsHeldThenPlayed() {
        var p = TonePolicy()
        XCTAssertEqual(p.want(CallTones.ringback), .hold(CallTones.ringback), "not played: that would activate the session")
        XCTAssertEqual(p.session(active: true), .play(CallTones.ringback), "released by didActivate")
    }

    /// Mid-call (the call-waiting beep): the session is already CallKit's.
    func testToneOnALiveSessionPlaysAtOnce() {
        var p = TonePolicy()
        XCTAssertEqual(p.session(active: true), .nothing)
        XCTAssertEqual(p.want(CallTones.callWaiting), .play(CallTones.callWaiting))
    }

    /// A tone held for a call that never got its session must not surface
    /// on the next one.
    func testDeactivationDropsAHeldTone() {
        var p = TonePolicy()
        _ = p.want(CallTones.ringback)
        XCTAssertEqual(p.session(active: false), .stop)
        XCTAssertEqual(p.session(active: true), .nothing, "nothing left over from the dead call")
    }

    /// Stopping a tone that was only ever held stops it for good.
    func testStopClearsAHeldTone() {
        var p = TonePolicy()
        _ = p.want(CallTones.ringback)
        XCTAssertEqual(p.want(nil), .stop)
        XCTAssertEqual(p.session(active: true), .nothing)
    }

    /// The last tone asked for wins, held or playing.
    func testLatestToneReplacesTheHeldOne() {
        var p = TonePolicy()
        _ = p.want(CallTones.ringback)
        XCTAssertEqual(p.want(CallTones.congestion), .hold(CallTones.congestion))
        XCTAssertEqual(p.session(active: true), .play(CallTones.congestion))
    }
}
