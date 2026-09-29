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
/// Started again once the audio settles. On the device the first engine
/// started on the output format iOS reported before voice processing took
/// over (44.1 kHz stereo against the call's 48 kHz mono); iOS then
/// reconfigured the hardware, in several steps, and AVAudioEngine stopped
/// itself, as it does on every configuration change, having heard nothing
/// (2026-09-28). Rebuilding at the first change was too soon: the next
/// engine found no microphone format while the hardware was still changing
/// (2026-09-29). So every change, a missing microphone or a stopped engine
/// starts it again half a second after the last of them.
@MainActor
final class DemoEcho {
    private var engine: AVAudioEngine?
    private var configChange: NSObjectProtocol?
    /// Whether the demo engine wants the echo on: a restart happens only
    /// while it does.
    private var wanted = false
    private var attempts = 0
    private var pendingStart: DispatchWorkItem?
    private static let maxAttempts = 6
    private static let settle: TimeInterval = 0.5
    /// After this many attempts with voice processing, try without: an echo
    /// that may repeat faintly on the speaker beats silence. A fallback.
    private static let attemptsWithVoiceProcessing = 3
    private var muted = false
    /// Microphone frames and peak level since the echo started, for the log.
    private let meter = Meter()
    var log: (String) -> Void = { _ in }

    /// How late the voice comes back: long enough to hear as an echo.
    private static let delay: TimeInterval = 0.4

    func set(running: Bool) {
        wanted = running
        if running {
            attempts = 0
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
        if attempts < Self.attemptsWithVoiceProcessing {
            do {
                try input.setVoiceProcessingEnabled(true)
            } catch {
                log("demo echo: no voice processing (\(error.localizedDescription)); echoing without it")
            }
        } else {
            log("demo echo: trying without voice processing")
        }
        let format = input.outputFormat(forBus: 0)
        guard format.sampleRate > 0, format.channelCount > 0 else {
            e.stop()
            startAgain(because: "no microphone format yet")
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
            MainActor.assumeIsolated { self?.restart(e, because: "the audio configuration changed") }
        }
        do {
            try e.start()
            engine = e
            log("demo echo: on (microphone \(format), output \(e.outputNode.inputFormat(forBus: 0)))")
            DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in
                guard let self, self.engine === e else { return }
                self.log("demo echo: after 2 s running \(e.isRunning), \(self.meter.summary)")
                // A backstop: a stop with no configuration change posted.
                if !e.isRunning { self.restart(e, because: "it stopped") }
            }
        } catch {
            tearDown(e)
            log("demo echo: could not start (\(error.localizedDescription))")
        }
    }

    /// Drop a stopped or reconfigured engine and start a fresh one.
    private func restart(_ e: AVAudioEngine, because reason: String) {
        guard engine === e else { return }
        tearDown(e)
        startAgain(because: reason)
    }

    /// Start again once the audio has been quiet for `settle`: a further
    /// change meanwhile pushes the start back. A few times at most.
    private func startAgain(because reason: String) {
        guard wanted else { return }
        pendingStart?.cancel()
        guard attempts < Self.maxAttempts else {
            log("demo echo: \(reason); given up after \(attempts) attempts")
            return
        }
        attempts += 1
        log("demo echo: \(reason); starting again in \(Self.settle) s (attempt \(attempts))")
        let work = DispatchWorkItem { [weak self] in
            MainActor.assumeIsolated { self?.start() }
        }
        pendingStart = work
        DispatchQueue.main.asyncAfter(deadline: .now() + Self.settle, execute: work)
    }

    private func stop() {
        pendingStart?.cancel()
        pendingStart = nil
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
