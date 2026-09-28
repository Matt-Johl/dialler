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
///
/// Rebuilt on a configuration change. On the device (2026-09-28) the engine
/// started on the output format iOS reported before voice processing took
/// over (44.1 kHz stereo against the call's 48 kHz mono), iOS then
/// reconfigured the hardware, and AVAudioEngine stopped itself, as it does
/// on every configuration change, having heard nothing. Apple's answer is
/// to rebuild the graph when the change is posted; by then the hardware is
/// in its call format.
@MainActor
final class DemoEcho {
    private var engine: AVAudioEngine?
    private var configChange: NSObjectProtocol?
    /// Whether the demo engine wants the echo on: a rebuild happens only
    /// while it does.
    private var wanted = false
    private var rebuilds = 0
    private static let maxRebuilds = 3
    private var muted = false
    /// Microphone frames and peak level since the echo started, for the log.
    private let meter = Meter()
    var log: (String) -> Void = { _ in }

    /// How late the voice comes back: long enough to hear as an echo.
    private static let delay: TimeInterval = 0.4

    func set(running: Bool) {
        wanted = running
        if running {
            rebuilds = 0
            start()
        } else {
            stop()
        }
    }

    func set(muted: Bool) {
        self.muted = muted
        engine?.mainMixerNode.outputVolume = muted ? 0 : 1
    }

    private func start() {
        guard engine == nil else { return }
        let e = AVAudioEngine()
        let input = e.inputNode
        do {
            try input.setVoiceProcessingEnabled(true)
        } catch {
            log("demo echo: no voice processing (\(error.localizedDescription)); echoing without it")
        }
        let format = input.outputFormat(forBus: 0)
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

        configChange = NotificationCenter.default.addObserver(forName: .AVAudioEngineConfigurationChange, object: e, queue: .main) { [weak self] _ in
            MainActor.assumeIsolated { self?.rebuild(e, because: "the audio configuration changed") }
        }
        do {
            try e.start()
            engine = e
            log("demo echo: on (microphone \(format), output \(e.outputNode.inputFormat(forBus: 0)))")
            DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in
                guard let self, self.engine === e else { return }
                self.log("demo echo: after 2 s running \(e.isRunning), \(self.meter.summary)")
                // A backstop: a stop with no configuration change posted.
                if !e.isRunning { self.rebuild(e, because: "it stopped") }
            }
        } catch {
            tearDown(e)
            log("demo echo: could not start (\(error.localizedDescription))")
        }
    }

    /// Replace a stopped or reconfigured engine with a fresh one, while the
    /// echo is still wanted, a few times at most.
    private func rebuild(_ e: AVAudioEngine, because reason: String) {
        guard wanted, engine === e else { return }
        tearDown(e)
        guard rebuilds < Self.maxRebuilds else {
            log("demo echo: \(reason); given up after \(rebuilds) rebuilds")
            return
        }
        rebuilds += 1
        log("demo echo: \(reason); rebuilding (\(rebuilds))")
        start()
    }

    private func stop() {
        guard let e = engine else { return }
        log("demo echo: off (\(meter.summary))")
        tearDown(e)
    }

    private func tearDown(_ e: AVAudioEngine) {
        if let configChange { NotificationCenter.default.removeObserver(configChange) }
        configChange = nil
        e.inputNode.removeTap(onBus: 0)
        e.stop()
        if engine === e { engine = nil }
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
