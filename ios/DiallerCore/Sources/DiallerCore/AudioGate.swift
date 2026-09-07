import Foundation

/// The manual-audio contract between CallKit and the media engine, as a
/// pure state machine so it can be unit-tested (SPEC §7.1 seam).
///
/// Rules (the ones WebRTC exposes as `useManualAudio` + `isAudioEnabled`):
///  - the engine starts HELD: it never starts audio on its own;
///  - the answer action answers the call and touches no audio;
///  - `didActivate` is the only thing that RELEASES audio;
///  - `didDeactivate` and a provider reset HOLD it again;
///  - call establishment issues no audio command: if released, the driver
///    starts the new units itself; if held, they wait.
public struct AudioGate: Equatable, Sendable {
    public enum Command: Equatable, Sendable {
        case hold
        case release
        case answer(String)
        case hangup(String)
    }

    public private(set) var held = true
    public private(set) var sessionActive = false

    public init() {}

    /// The media stack came up.
    public mutating func engineStarted() -> [Command] {
        held = true
        return [.hold]
    }

    /// CXAnswerCallAction: answer now, fulfill; no audio work.
    public func answerAction(callID: String) -> [Command] {
        [.answer(callID)]
    }

    /// provider(_:didActivate:)
    public mutating func didActivate() -> [Command] {
        sessionActive = true
        held = false
        return [.release]
    }

    /// provider(_:didDeactivate:)
    public mutating func didDeactivate() -> [Command] {
        sessionActive = false
        held = true
        return [.hold]
    }

    /// The SIP call established; the driver has just allocated its units.
    public func callEstablished() -> [Command] {
        []
    }

    /// providerDidReset: CallKit dropped every call.
    public mutating func providerReset(calls: [String]) -> [Command] {
        sessionActive = false
        held = true
        return calls.map { .hangup($0) } + [.hold]
    }
}
