import AVFoundation

/// The demo's far end (see `Demo` in DiallerCore): the microphone played
/// back a moment late, the way a SIP echo test sounds. `DemoCallEngine`
/// says when: only while a call is live, not held, and CallKit has
/// activated the call's audio session — CallKit owns the session, and
/// nothing may play before it hands it over.
///
/// Voice processing is on, as for real calls: its echo cancellation removes
/// what the speaker plays from what the microphone hears, so the caller
/// hears themselves once, not over and over.
@MainActor
final class DemoEcho {
    private var engine: AVAudioEngine?
    private var muted = false
    /// Microphone frames seen since the echo started, and their peak: the
    /// log says whether audio reached the echo at all (2026-09-28: the
    /// echo was silent on the device, with nothing yet to say why).
    private let meter = Meter()
    var log: (String) -> Void = { _ in }

    /// How late the voice comes back: long enough to hear as an echo.
    private static let delay: TimeInterval = 0.4

    func set(running: Bool) {
        running ? start() : stop()
    }

    func set(muted: Bool) {
        self.muted = muted
        engine?.mainMixerNode.outputVolume = muted ? 0 : 1
    }

    private func start() {
        guard engine == nil else { return }
        let session = AVAudioSession.sharedInstance()
        log("demo echo: starting (session \(session.category.rawValue)/\(session.mode.rawValue), \(Int(session.sampleRate)) Hz, in \(session.currentRoute.inputs.first?.portName ?? "none"), out \(session.currentRoute.outputs.first?.portName ?? "none"))")
        let e = AVAudioEngine()
        let input = e.inputNode
        do {
            try input.setVoiceProcessingEnabled(true)
        } catch {
            log("demo echo: no voice processing (\(error.localizedDescription)); echoing without it")
        }
        let format = input.outputFormat(forBus: 0)
        log("demo echo: microphone format \(format)")
        guard format.sampleRate > 0, format.channelCount > 0 else {
            log("demo echo: no microphone input")
            return
        }
        let delay = AVAudioUnitDelay()
        delay.delayTime = Self.delay
        delay.feedback = 0
        delay.wetDryMix = 100
        e.attach(delay)
        e.connect(input, to: delay, format: format)
        e.connect(delay, to: e.mainMixerNode, format: format)
        e.mainMixerNode.outputVolume = muted ? 0 : 1
        meter.reset()
        input.installTap(onBus: 0, bufferSize: 1024, format: format, block: meter.tap)
        do {
            try e.start()
            engine = e
            log("demo echo: on (running \(e.isRunning), output \(e.outputNode.inputFormat(forBus: 0)))")
            DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in
                guard let self, self.engine === e else { return }
                self.log("demo echo: after 2 s running \(e.isRunning), \(self.meter.summary)")
            }
        } catch {
            input.removeTap(onBus: 0)
            log("demo echo: could not start (\(error.localizedDescription))")
        }
    }

    private func stop() {
        guard let e = engine else { return }
        e.inputNode.removeTap(onBus: 0)
        e.stop()
        engine = nil
        log("demo echo: off (\(meter.summary))")
    }

    /// Frames and peak level of the microphone, from the audio thread. Not
    /// main-actor isolated, so the tap it hands out may run there.
    private final class Meter: @unchecked Sendable {
        private let lock = NSLock()
        private var frames = 0
        private var peak: Float = 0

        func reset() { lock.withLock { frames = 0; peak = 0 } }

        var tap: AVAudioNodeTapBlock { { [self] buffer, _ in add(buffer) } }

        func add(_ buffer: AVAudioPCMBuffer) {
            guard let data = buffer.floatChannelData else { return }
            var p: Float = 0
            for i in 0..<Int(buffer.frameLength) { p = max(p, abs(data[0][i])) }
            lock.withLock { frames += Int(buffer.frameLength); peak = max(peak, p) }
        }

        var summary: String {
            lock.withLock {
                let db = peak > 0 ? Int((20 * log10(Double(peak))).rounded()) : -120
                return "microphone frames \(frames), peak \(db) dBFS"
            }
        }
    }
}
