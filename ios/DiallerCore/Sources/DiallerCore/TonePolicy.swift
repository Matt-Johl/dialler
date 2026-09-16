import Foundation

/// When a call-progress tone may actually be played (plan Phase J).
///
/// This is one rule, and it is load-bearing: **a tone must never be what
/// activates the audio session.** The session belongs to CallKit, which
/// activates it for a call and tells the app through `didActivate`. But
/// ring-back is asked for when the far end's 180 arrives, and that beats
/// `didActivate` (measured: by 154 ms, `make sim-call`) — and
/// `AVAudioPlayer.play()` activates the shared session itself when it is
/// not already active. Playing straight away therefore takes the session
/// out of CallKit's hands, the same class of fault as the audio driver
/// building its CoreAudio units ahead of activation in Phase 1, and it
/// cost the app its audio on every call (2026-09-15).
///
/// The rule lives here, in a pure value, rather than inside the app's
/// `AVAudioPlayer` wrapper, precisely because it went wrong once: the app
/// target has no tests, so a rule kept there is a rule nobody checks.
public struct TonePolicy: Equatable, Sendable {
    /// What the player should do about it.
    public enum Action: Equatable, Sendable {
        /// Start this tone now.
        case play(CallTones.Tone)
        /// Keep it until the session is active; play nothing yet.
        case hold(CallTones.Tone)
        /// Stop whatever is playing.
        case stop
        case nothing
    }

    private var sessionActive = false
    private var pending: CallTones.Tone?

    public init() {}

    /// A tone was asked for, or nil to stop the one playing.
    public mutating func want(_ tone: CallTones.Tone?) -> Action {
        guard let tone else {
            pending = nil
            return .stop
        }
        if sessionActive {
            pending = nil
            return .play(tone)
        }
        pending = tone
        return .hold(tone)
    }

    /// CallKit's `didActivate` (true) / `didDeactivate` (false). Activating
    /// releases a tone that was waiting; deactivating drops it — the call
    /// it belonged to is gone with the session.
    public mutating func session(active: Bool) -> Action {
        sessionActive = active
        guard active else {
            pending = nil
            return .stop
        }
        guard let tone = pending else { return .nothing }
        pending = nil
        return .play(tone)
    }
}
