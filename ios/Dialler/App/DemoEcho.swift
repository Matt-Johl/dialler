import AudioToolbox
import AVFoundation

/// The demo's far end (see `Demo` in DiallerCore): the microphone played
/// back 0.4 s late, the way a SIP echo test sounds. `DemoCallEngine` says
/// when: while a call is live and CallKit has activated the call's audio
/// session. CallKit owns the session: the unit is created only after it is
/// handed over and disposed of when it is taken back (at hold, too).
///
/// One VoiceProcessingIO unit, the way the real engine's audio driver works
/// (ios/vendor/patches/audiounit): voice processing's gain control brings
/// the voice up to call level, and its echo cancellation keeps the speaker
/// from feeding the echo back into itself. The formats are set on the
/// unit's app-facing sides, so it converts to whatever the hardware settles
/// on, and it keeps running through the hardware switch into voice
/// processing that follows its start.
///
/// Not AVAudioEngine: it stops itself on every configuration change, and
/// enabling voice processing is one. On the device every such engine
/// stopped moments after starting, and only a fallback without voice
/// processing ever played: quiet (no gain control), and slow to come back
/// after hold, where CallKit deactivates the session and the whole sequence
/// ran again (2026-09-29 log).
@MainActor
final class DemoEcho {
    private var unit: AudioUnit?
    private var line: DelayLine?
    private var muted = false
    var log: (String) -> Void = { _ in }

    private static let delay: TimeInterval = 0.4
    /// The most frames the unit may ask for at once; the scratch buffer is
    /// sized for it, so the audio thread never allocates.
    private static let maxFrames: UInt32 = 4096

    func set(running: Bool) {
        running ? start() : stop()
    }

    func set(muted: Bool) {
        self.muted = muted
        line?.muted = muted
    }

    private func start() {
        guard unit == nil else { return }
        let rate = AVAudioSession.sharedInstance().sampleRate
        var desc = AudioComponentDescription(componentType: kAudioUnitType_Output,
                                             componentSubType: kAudioUnitSubType_VoiceProcessingIO,
                                             componentManufacturer: kAudioUnitManufacturer_Apple,
                                             componentFlags: 0, componentFlagsMask: 0)
        guard let component = AudioComponentFindNext(nil, &desc) else {
            log("demo echo: no voice processing unit")
            return
        }
        var created: AudioUnit?
        guard AudioComponentInstanceNew(component, &created) == noErr, let au = created else {
            log("demo echo: could not create the voice processing unit")
            return
        }
        let line = DelayLine(frames: Int(rate * Self.delay), maxFrames: Int(Self.maxFrames))
        line.unit = au
        line.muted = muted

        // Mono 32-bit float at the session's rate, on the side the app
        // reads (the microphone's output) and the side it writes (the
        // speaker's input).
        var format = AudioStreamBasicDescription(
            mSampleRate: rate, mFormatID: kAudioFormatLinearPCM,
            mFormatFlags: kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked | kAudioFormatFlagsNativeEndian,
            mBytesPerPacket: 4, mFramesPerPacket: 1, mBytesPerFrame: 4,
            mChannelsPerFrame: 1, mBitsPerChannel: 32, mReserved: 0)
        var enable: UInt32 = 1
        var maxFrames = Self.maxFrames
        var callback = AURenderCallbackStruct(inputProc: demoEchoRender,
                                              inputProcRefCon: Unmanaged.passUnretained(line).toOpaque())
        let steps: [(String, () -> OSStatus)] = [
            ("enable the microphone", {
                AudioUnitSetProperty(au, kAudioOutputUnitProperty_EnableIO, kAudioUnitScope_Input, Bus.microphone,
                                     &enable, UInt32(MemoryLayout<UInt32>.size))
            }),
            ("set the microphone format", {
                AudioUnitSetProperty(au, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Output, Bus.microphone,
                                     &format, UInt32(MemoryLayout<AudioStreamBasicDescription>.size))
            }),
            ("set the speaker format", {
                AudioUnitSetProperty(au, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Input, Bus.speaker,
                                     &format, UInt32(MemoryLayout<AudioStreamBasicDescription>.size))
            }),
            ("set the slice size", {
                AudioUnitSetProperty(au, kAudioUnitProperty_MaximumFramesPerSlice, kAudioUnitScope_Global, 0,
                                     &maxFrames, UInt32(MemoryLayout<UInt32>.size))
            }),
            ("set the render callback", {
                AudioUnitSetProperty(au, kAudioUnitProperty_SetRenderCallback, kAudioUnitScope_Input, Bus.speaker,
                                     &callback, UInt32(MemoryLayout<AURenderCallbackStruct>.size))
            }),
            ("initialise", { AudioUnitInitialize(au) }),
            ("start", { AudioOutputUnitStart(au) }),
        ]
        for (step, run) in steps {
            let status = run()
            guard status == noErr else {
                log("demo echo: could not \(step) (\(status))")
                AudioUnitUninitialize(au)
                AudioComponentInstanceDispose(au)
                return
            }
        }
        unit = au
        self.line = line
        log("demo echo: on (\(Int(rate)) Hz, voice processing)")
    }

    private func stop() {
        guard let au = unit, let line else { return }
        // Stop returns once the render callback has finished; only then may
        // the delay line it reads go.
        AudioOutputUnitStop(au)
        AudioUnitUninitialize(au)
        AudioComponentInstanceDispose(au)
        unit = nil
        self.line = nil
        log("demo echo: off (\(line.summary))")
    }

    private enum Bus {
        static let speaker: AudioUnitElement = 0
        static let microphone: AudioUnitElement = 1
    }
}

/// The render callback: a C function, so it captures nothing and finds its
/// delay line through the reference constant.
private let demoEchoRender: AURenderCallback = { refCon, flags, timestamp, _, frames, ioData in
    Unmanaged<DelayLine>.fromOpaque(refCon).takeUnretainedValue()
        .render(flags: flags, timestamp: timestamp, frames: frames, into: ioData)
}

/// The audio thread's side: pulls the microphone into a scratch buffer and
/// plays what it heard `frames` samples ago. Everything is allocated up
/// front; the callback neither allocates, locks nor logs.
private final class DelayLine: @unchecked Sendable {
    var unit: AudioUnit?
    /// Written on the main thread, read on the audio thread; a word-sized
    /// flag, and a stale read costs one buffer of sound.
    var muted = false

    private let delay: UnsafeMutablePointer<Float>
    private let length: Int
    private var position = 0
    private let scratch: UnsafeMutablePointer<Float>
    private let maxFrames: Int
    /// For the log, read only after the unit has stopped.
    private var heard = 0
    private var peak: Float = 0

    init(frames: Int, maxFrames: Int) {
        length = max(frames, 1)
        delay = .allocate(capacity: length)
        delay.initialize(repeating: 0, count: length)
        self.maxFrames = maxFrames
        scratch = .allocate(capacity: maxFrames)
        scratch.initialize(repeating: 0, count: maxFrames)
    }

    deinit {
        delay.deallocate()
        scratch.deallocate()
    }

    func render(flags: UnsafeMutablePointer<AudioUnitRenderActionFlags>, timestamp: UnsafePointer<AudioTimeStamp>,
                frames: UInt32, into ioData: UnsafeMutablePointer<AudioBufferList>?) -> OSStatus {
        guard let ioData, let out = UnsafeMutableAudioBufferListPointer(ioData).first?.mData?.assumingMemoryBound(to: Float.self) else {
            return noErr
        }
        let n = Int(frames)
        guard let unit, n <= maxFrames else {
            out.update(repeating: 0, count: n)
            return noErr
        }
        var input = AudioBufferList(mNumberBuffers: 1, mBuffers: AudioBuffer(
            mNumberChannels: 1, mDataByteSize: UInt32(n * MemoryLayout<Float>.size), mData: UnsafeMutableRawPointer(scratch)))
        guard AudioUnitRender(unit, flags, timestamp, 1, frames, &input) == noErr else {
            out.update(repeating: 0, count: n)
            return noErr
        }
        let silent = muted
        for i in 0..<n {
            let sample = scratch[i]
            peak = max(peak, abs(sample))
            out[i] = silent ? 0 : delay[position]
            delay[position] = sample
            position = position + 1 == length ? 0 : position + 1
        }
        heard += n
        return noErr
    }

    var summary: String {
        let db = peak > 0 ? Int((20 * log10(Double(peak))).rounded()) : -120
        return "microphone frames \(heard), peak \(db) dBFS"
    }
}
