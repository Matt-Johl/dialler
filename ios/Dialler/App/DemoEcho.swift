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
        do {
            try e.start()
            engine = e
            log("demo echo: on")
        } catch {
            log("demo echo: could not start (\(error.localizedDescription))")
        }
    }

    private func stop() {
        guard let e = engine else { return }
        e.stop()
        engine = nil
        log("demo echo: off")
    }
}
