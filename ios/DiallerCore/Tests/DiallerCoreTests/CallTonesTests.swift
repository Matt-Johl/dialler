import XCTest
@testable import DiallerCore

final class CallTonesTests: XCTestCase {
    /// A canonical 16-bit mono WAV of exactly the cadence's length.
    func testWavHeaderAndLength() {
        let rate = 8000
        let tone = CallTones.Tone(hz: 425, segments: [.on(0.1), .off(0.1), .on(0.1)])
        let d = CallTones.wav(tone, sampleRate: rate)
        XCTAssertEqual(String(decoding: d[0..<4], as: UTF8.self), "RIFF")
        XCTAssertEqual(String(decoding: d[8..<12], as: UTF8.self), "WAVE")
        XCTAssertEqual(String(decoding: d[36..<40], as: UTF8.self), "data")
        let samples = Int(tone.duration * Double(rate))
        XCTAssertEqual(d.count, 44 + samples * 2)
        XCTAssertEqual(le32(d, 4), UInt32(36 + samples * 2), "RIFF size counts everything after it")
        XCTAssertEqual(le32(d, 24), UInt32(rate), "sample rate")
        XCTAssertEqual(le16(d, 22), 1, "mono")
        XCTAssertEqual(le16(d, 34), 16, "16-bit")
    }

    /// Tone where the cadence says tone, silence where it says silence.
    func testCadenceIsAudibleThenSilent() {
        let rate = 8000
        let tone = CallTones.Tone(hz: 425, segments: [.on(0.1), .off(0.1), .on(0.1)])
        let d = CallTones.wav(tone, sampleRate: rate)
        func rms(_ from: Double, _ to: Double) -> Double {
            let a = 44 + Int(from * Double(rate)) * 2, b = 44 + Int(to * Double(rate)) * 2
            var sum = 0.0
            var i = a
            while i + 1 < b {
                let v = Double(Int16(littleEndian: d.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: i, as: Int16.self) }))
                sum += v * v
                i += 2
            }
            return (sum / Double(max(1, (b - a) / 2))).squareRoot()
        }
        XCTAssertGreaterThan(rms(0.03, 0.07), 1000, "first burst is audible")
        XCTAssertEqual(rms(0.12, 0.18), 0, "the gap is silent")
        XCTAssertGreaterThan(rms(0.23, 0.27), 1000, "second burst is audible")
    }

    /// The bursts are ramped, so the earpiece does not click.
    func testBurstsAreFadedAtTheEnds() {
        let rate = 48000
        let d = CallTones.wav(CallTones.Tone(hz: 425, segments: [.on(0.1)]), sampleRate: rate)
        let first = abs(Int(Int16(littleEndian: d.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 44, as: Int16.self) })))
        let last = abs(Int(Int16(littleEndian: d.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: d.count - 2, as: Int16.self) })))
        XCTAssertLessThan(first, 100, "starts from silence")
        XCTAssertLessThan(last, 100, "ends in silence")
    }

    /// The call-waiting tone is short, quiet and repeats on a long interval:
    /// it goes into the ear of someone already talking.
    func testCallWaitingToneShape() {
        let t = CallTones.callWaiting
        XCTAssertEqual(t.everySeconds, 5)
        XCTAssertEqual(t.duration, 0.3, accuracy: 0.001)
        XCTAssertEqual(t.segments.filter(\.on).count, 2, "two bursts")
    }

    /// The ETSI cadences the rest of the plan is built on: every tone is
    /// 425 Hz, and each is told from the others by its rhythm alone —
    /// ring-back 1 s on / 4 s off, busy 0.5/0.5, congestion 0.25/0.25.
    func testProgressToneCadences() {
        for t in [CallTones.ringback, CallTones.busy, CallTones.congestion] {
            XCTAssertEqual(t.hz, 425)
            XCTAssertEqual(t.hz2, 0, "single tone")
            XCTAssertEqual(t.segments.count, 1, "one burst; the gap is the repeat interval")
        }
        XCTAssertEqual(CallTones.ringback.duration, 1, accuracy: 0.001)
        XCTAssertEqual(CallTones.ringback.everySeconds, 5)
        XCTAssertEqual(CallTones.busy.duration, 0.5, accuracy: 0.001)
        XCTAssertEqual(CallTones.busy.everySeconds, 1)
        XCTAssertEqual(CallTones.congestion.duration, 0.25, accuracy: 0.001)
        XCTAssertEqual(CallTones.congestion.everySeconds, 0.5)
    }

    /// Ring-back runs until the call is answered or given up on; a failure
    /// tone stops by itself, so the phone does not beep on and on.
    func testOnlyFailureTonesAreBounded() {
        XCTAssertNil(CallTones.ringback.maxSeconds)
        XCTAssertNil(CallTones.callWaiting.maxSeconds)
        XCTAssertEqual(CallTones.busy.maxSeconds, 4)
        XCTAssertEqual(CallTones.congestion.maxSeconds, 3)
    }

    /// Busy for the busy family, congestion for every other refusal, and
    /// silence for an ending that is nobody's fault or is our own doing.
    func testFailureToneForStatus() {
        for s in [486, 600, 603] { XCTAssertEqual(CallTones.failure(status: s), CallTones.busy, "\(s)") }
        for s in [404, 408, 480, 503, 606] { XCTAssertEqual(CallTones.failure(status: s), CallTones.congestion, "\(s)") }
        XCTAssertNil(CallTones.failure(status: 0), "a BYE or a local error")
        XCTAssertNil(CallTones.failure(status: 487), "our own CANCEL")
        XCTAssertNil(CallTones.failure(status: 200), "not a failure at all")
    }

    func le32(_ d: Data, _ at: Int) -> UInt32 { d.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: at, as: UInt32.self) }.littleEndian }
    func le16(_ d: Data, _ at: Int) -> UInt16 { d.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: at, as: UInt16.self) }.littleEndian }
}
