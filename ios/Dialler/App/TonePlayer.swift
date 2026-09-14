import AVFoundation
import DiallerCore
import Foundation

/// Plays call-progress tones into the call's own audio session (plan
/// Phase J; the call-waiting tone is its first user).
///
/// The session belongs to CallKit and is already active and configured for
/// the call when a tone is wanted, so nothing here touches its category or
/// activation: an `AVAudioPlayer` on the live session mixes with baresip's
/// audio unit and comes out of whatever the call is using — earpiece,
/// speaker or a headset. Tones that recur (call waiting) are repeated by a
/// timer rather than rendered as minutes of mostly-silent PCM.
@MainActor
final class TonePlayer {
    private var player: AVAudioPlayer?
    private var repeater: Timer?
    private var log: (String) -> Void

    init(log: @escaping (String) -> Void = { _ in }) {
        self.log = log
    }

    /// Start `tone`, repeating on its own interval until `stop()`.
    func start(_ tone: CallTones.Tone) {
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
        } catch {
            log("tone: could not play (\(error.localizedDescription))")
        }
    }

    func stop() {
        repeater?.invalidate()
        repeater = nil
        player?.stop()
        player = nil
    }
}
