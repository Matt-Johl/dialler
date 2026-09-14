import Foundation

/// Call-progress tones, rendered as PCM so the app can play them through
/// the call's own audio session (plan Phase J).
///
/// Tones are generated here rather than taken from the SIP stack: the audio
/// driver is one VoiceProcessingIO instance owned by baresip, and a second
/// player inside it would fight for the same unit. Everything in this file
/// is pure — frequencies, cadence and PCM — so the plan is a data change and
/// the rendering is unit-tested.
///
/// The default plan follows ETSI/ITU-T E.180 practice (425 Hz); a country
/// plan is a different table, not different code.
public enum CallTones {
    /// One step of a cadence: `seconds` of tone (`on`) or of silence.
    public struct Segment: Equatable, Sendable {
        public var seconds: Double
        public var on: Bool
        public static func on(_ s: Double) -> Segment { Segment(seconds: s, on: true) }
        public static func off(_ s: Double) -> Segment { Segment(seconds: s, on: false) }
    }

    public struct Tone: Equatable, Sendable {
        /// Tone frequency, and a second one for a dual tone (0 = single).
        public var hz: Double
        public var hz2: Double
        /// One period of the cadence; the player repeats it if it repeats.
        public var segments: [Segment]
        /// How often the period is repeated, for tones that recur on a long
        /// interval (call waiting). Nil = play the period once.
        public var everySeconds: Double?

        public init(hz: Double, hz2: Double = 0, segments: [Segment], everySeconds: Double? = nil) {
            self.hz = hz
            self.hz2 = hz2
            self.segments = segments
            self.everySeconds = everySeconds
        }

        public var duration: Double { segments.reduce(0) { $0 + $1.seconds } }
    }

    /// Call waiting: two short bursts into the ear of someone already on a
    /// call, repeated while the second call rings — what a desk phone
    /// injects and what the caller never hears. Deliberately short and
    /// quiet: the phone is against someone's ear.
    public static let callWaiting = Tone(hz: 425, segments: [.on(0.1), .off(0.1), .on(0.1)], everySeconds: 5)

    /// A 16-bit mono WAV of one cadence period, ready for AVAudioPlayer.
    /// Each burst is faded in and out over `fade` seconds so it does not
    /// click in the earpiece.
    public static func wav(_ tone: Tone, sampleRate: Int = 48000, amplitude: Double = 0.2, fade: Double = 0.005) -> Data {
        var samples: [Int16] = []
        samples.reserveCapacity(Int(tone.duration * Double(sampleRate)) + 1)
        var phase = 0.0, phase2 = 0.0
        let step = 2 * Double.pi * tone.hz / Double(sampleRate)
        let step2 = 2 * Double.pi * tone.hz2 / Double(sampleRate)
        let fadeSamples = max(1, Int(fade * Double(sampleRate)))
        for segment in tone.segments {
            let n = Int(segment.seconds * Double(sampleRate))
            for i in 0..<n {
                guard segment.on else {
                    samples.append(0)
                    continue
                }
                var v = sin(phase)
                if tone.hz2 > 0 { v = (v + sin(phase2)) / 2 }
                phase += step
                phase2 += step2
                // Ramp the ends of the burst.
                let ramp = min(1.0, Double(min(i, max(0, n - 1 - i))) / Double(fadeSamples))
                samples.append(Int16(max(-1, min(1, v * amplitude * ramp)) * 32767))
            }
        }
        return riff(samples, sampleRate: sampleRate)
    }

    /// Minimal canonical RIFF/WAVE, 16-bit mono PCM.
    static func riff(_ samples: [Int16], sampleRate: Int) -> Data {
        var d = Data()
        func str(_ s: String) { d.append(contentsOf: Array(s.utf8)) }
        func u32(_ v: UInt32) { withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) } }
        func u16(_ v: UInt16) { withUnsafeBytes(of: v.littleEndian) { d.append(contentsOf: $0) } }
        let bytes = UInt32(samples.count * 2)
        str("RIFF"); u32(36 + bytes); str("WAVE")
        str("fmt "); u32(16); u16(1); u16(1)
        u32(UInt32(sampleRate)); u32(UInt32(sampleRate * 2)); u16(2); u16(16)
        str("data"); u32(bytes)
        for s in samples { withUnsafeBytes(of: s.littleEndian) { d.append(contentsOf: $0) } }
        return d
    }
}
