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

    func le32(_ d: Data, _ at: Int) -> UInt32 { d.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: at, as: UInt32.self) }.littleEndian }
    func le16(_ d: Data, _ at: Int) -> UInt16 { d.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: at, as: UInt16.self) }.littleEndian }
}
