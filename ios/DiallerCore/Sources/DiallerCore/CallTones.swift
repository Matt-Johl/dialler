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
        /// How long the repeat goes on before the tone stops by itself. Nil
        /// = until the player is stopped. A failure tone is bounded (nobody
        /// wants a phone that beeps until it is picked up); ring-back is
        /// not — it ends when the call is answered, cancelled or times out.
        public var maxSeconds: Double?

        public init(hz: Double, hz2: Double = 0, segments: [Segment], everySeconds: Double? = nil, maxSeconds: Double? = nil) {
            self.hz = hz
            self.hz2 = hz2
            self.segments = segments
            self.everySeconds = everySeconds
            self.maxSeconds = maxSeconds
        }

        public var duration: Double { segments.reduce(0) { $0 + $1.seconds } }
    }

    /// Call waiting: two short bursts into the ear of someone already on a
    /// call, repeated while the second call rings — what a desk phone
    /// injects and what the caller never hears. Deliberately short and
    /// quiet: the phone is against someone's ear.
    public static let callWaiting = Tone(hz: 425, segments: [.on(0.1), .off(0.1), .on(0.1)], everySeconds: 5)

    /// Ring-back: what the caller hears while the far end is being alerted.
    /// Nothing else produces it on this path — iOS plays no tone for a VoIP
    /// app's outgoing call, and a 180 carries no media, so without this the
    /// caller hears silence from the moment they dial until the answer.
    /// ETSI cadence: 1 s on, 4 s off.
    ///
    /// Not played when the far end sends early media instead (183 with SDP,
    /// which is how a PBX delivers its own ring-back or an announcement):
    /// that audio is already in the caller's ear and a second tone over it
    /// is the classic double ring-back.
    public static let ringback = Tone(hz: 425, segments: [.on(1)], everySeconds: 5)

    /// Busy: the callee is on the phone and took neither call. ETSI cadence,
    /// 0.5 s on / 0.5 s off, for four seconds — long enough to be recognised
    /// as busy rather than a glitch, short enough not to nag.
    public static let busy = Tone(hz: 425, segments: [.on(0.5)], everySeconds: 1, maxSeconds: 4)

    /// Congestion / number unobtainable: the call failed for any other
    /// reason — nobody of that name, nothing to wake, the PBX refused it.
    /// ETSI cadence, 0.25 s on / 0.25 s off; faster than busy, which is how
    /// the two are told apart by ear.
    public static let congestion = Tone(hz: 425, segments: [.on(0.25)], everySeconds: 0.5, maxSeconds: 3)

    /// The tone for an outgoing call that ended with SIP status `status`,
    /// or nil when the ending deserves none.
    ///
    /// Nothing is played for a call that simply ended: status 0 is a BYE or
    /// a local error (baresip reports those with text alone), and 487 is the
    /// answer to our own CANCEL — the user hung up, and a tone in reply to
    /// their own thumb is noise. Everything from 400 up is a refusal the
    /// caller should hear.
    public static func failure(status: Int) -> Tone? {
        switch status {
        case 486, 600, 603: return busy // Busy Here / Busy Everywhere / Decline
        case 487: return nil            // Request Terminated: our own CANCEL
        case 400...: return congestion
        default: return nil
        }
    }

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
