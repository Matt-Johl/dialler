import DiallerProtocol
import Foundation

/// One engine for the controller, two behind it: the SIP engine, or the
/// demo's (appstore.md, decision 2). The controller is built once, with one
/// engine, and installs its callbacks on it at that moment; this passes them
/// on to both, and every call to whichever is in use. Switch only while no
/// call is up (entering or leaving demo mode).
public final class CallEngineSwitch: CallEngine, @unchecked Sendable {
    public let primary: CallEngine
    public let alternate: CallEngine
    private let lock = NSLock()
    private var usingAlternate = false

    public init(primary: CallEngine, alternate: CallEngine) {
        self.primary = primary
        self.alternate = alternate
    }

    public var usesAlternate: Bool { lock.withLock { usingAlternate } }

    public func use(alternate: Bool) {
        lock.withLock { usingAlternate = alternate }
    }

    private var active: CallEngine { usesAlternate ? alternate : primary }

    // Callbacks reach the controller from either engine.
    public var onIncomingCall: ((String, String, String?, String?) -> Void)? {
        didSet { primary.onIncomingCall = onIncomingCall; alternate.onIncomingCall = onIncomingCall }
    }
    public var onCallEnded: ((String, String, Int) -> Void)? {
        didSet { primary.onCallEnded = onCallEnded; alternate.onCallEnded = onCallEnded }
    }
    public var onOutgoingRinging: ((String, Bool) -> Void)? {
        didSet { primary.onOutgoingRinging = onOutgoingRinging; alternate.onOutgoingRinging = onOutgoingRinging }
    }
    public var onCallEstablished: ((String) -> Void)? {
        didSet { primary.onCallEstablished = onCallEstablished; alternate.onCallEstablished = onCallEstablished }
    }
    public var onTransferFailed: ((String, String) -> Void)? {
        didSet { primary.onTransferFailed = onTransferFailed; alternate.onTransferFailed = onTransferFailed }
    }

    // Everything else goes to the engine in use.
    public func setCredentials(username: String, password: String) { active.setCredentials(username: username, password: password) }
    public func setServerTrust(pin: String?, acceptAnyCertificate: Bool) { active.setServerTrust(pin: pin, acceptAnyCertificate: acceptAnyCertificate) }
    public func register(user: String, sip: SIPTarget) { active.register(user: user, sip: sip) }
    public func resetRegistration() { active.resetRegistration() }
    @discardableResult
    public func answer(engineCallID: String) -> Bool { active.answer(engineCallID: engineCallID) }
    public func reject(engineCallID: String) { active.reject(engineCallID: engineCallID) }
    @discardableResult
    public func dial(callID: String, to target: String) -> String? { active.dial(callID: callID, to: target) }
    public func hangup(engineCallID: String) { active.hangup(engineCallID: engineCallID) }
    public func setMuted(_ muted: Bool) { active.setMuted(muted) }
    public func setHeld(engineCallID: String, _ held: Bool) { active.setHeld(engineCallID: engineCallID, held) }
    public func transfer(engineCallID: String, to target: String) { active.transfer(engineCallID: engineCallID, to: target) }
    public func audioSessionActivated() { active.audioSessionActivated() }
    public func audioSessionDeactivated() { active.audioSessionDeactivated() }
}
