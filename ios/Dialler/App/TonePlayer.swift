import AVFoundation
import DiallerCore
import Foundation

/// Plays call-progress tones into the call's own audio session (plan
/// Phase J; the call-waiting tone is its first user).
///
/// The session belongs to CallKit, so nothing here touches its category or
/// activation: an `AVAudioPlayer` on the live session mixes with baresip's
/// audio unit and comes out of whatever the call is using — earpiece,
/// speaker or a headset. Tones that recur (call waiting) are repeated by a
/// timer rather than rendered as minutes of mostly-silent PCM.
///
/// **A tone is never allowed to be what activates the session** — the rule
/// and the reason are `TonePolicy` in DiallerCore, which is where it is
/// tested. This class only does what the policy says.
@MainActor
final class TonePlayer {
    private var player: AVAudioPlayer?
    private var repeater: Timer?
    /// Stops a tone that has a bounded life (busy, congestion).
    private var limit: Timer?
    private var log: (String) -> Void
    private var policy = TonePolicy()

    init(log: @escaping (String) -> Void = { _ in }) {
        self.log = log
    }

    /// CallKit's `didActivate` / `didDeactivate`.
    func session(active: Bool) { apply(policy.session(active: active)) }

    /// Ask for `tone`, or nil to stop. What actually happens is the
    /// policy's decision, not this class's.
    func want(_ tone: CallTones.Tone?) { apply(policy.want(tone)) }

    private func apply(_ action: TonePolicy.Action) {
        switch action {
        case .play(let tone): play(tone)
        case .hold: log("tone: asked for before CallKit activated the session; held until it does")
        case .stop: stop()
        case .nothing: break
        }
    }

    /// Start `tone`, repeating on its own interval until `stop()` — or
    /// until its own `maxSeconds` is up, for the tones that must not go on
    /// for ever (busy, congestion). Only the policy calls this.
    private func play(_ tone: CallTones.Tone) {
        stop()
        let data = CallTones.wav(tone)
        do {
            let p = try AVAudioPlayer(data: data)
            p.prepareToPlay()
            player = p
            p.play()
            if let every = tone.everySeconds {
                repeater = Timer.scheduledTimer(withTimeInterval: every, repeats: true) { [weak self] _ in
                    Task { @MainActor in self?.player?.play() }
                }
            }
            if let max = tone.maxSeconds {
                limit = Timer.scheduledTimer(withTimeInterval: max, repeats: false) { [weak self] _ in
                    Task { @MainActor in self?.stop() }
                }
            }
        } catch {
            log("tone: could not play (\(error.localizedDescription))")
        }
    }

    private func stop() {
        repeater?.invalidate()
        repeater = nil
        limit?.invalidate()
        limit = nil
        player?.stop()
        player = nil
    }
}
