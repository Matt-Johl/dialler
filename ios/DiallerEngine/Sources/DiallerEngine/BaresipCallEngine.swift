import CBaresip
import DiallerCore
import DiallerProtocol
import Foundation
import os

/// The real `CallEngine` (SPEC §5 component 6): wraps libbaresip via the
/// CBaresip shim.
///
/// Lifecycle: `register` is called as soon as the gateway welcome names our
/// SIP account, so the phone stays registered while the app runs and INVITEs
/// reach it directly (SPEC §2 foreground path). An INVITE is reported through
/// `onIncomingCall`; the controller rings CallKit (unless a wake already
/// did) and, on the user's answer, calls `prepareForIncomingCall`, which
/// answers the pending INVITE — or arms an auto-answer if it has not arrived
/// yet (wake path: we register on demand and the server then dials us).
///
/// Audio is baresip's `audiounit` module; CallKit activates the audio
/// session on answer, before the INVITE round trip completes.
///
/// Phase 1 scope: one call at a time (call_max_calls 1).
public final class BaresipCallEngine: CallEngine {
    public enum State: Equatable, Sendable {
        case idle, starting, registering, registered, ringing, inCall, failed(String)
    }

    public var onIncomingCall: ((String) -> Void)?
    public var onCallEnded: ((String) -> Void)?
    /// Observed by the app for status display.
    public var onStateChange: ((State) -> Void)?
    public var log: (String) -> Void = { _ in }

    private let logger = Logger(subsystem: DiallerIDs.bundlePrefix, category: "engine")
    private let lock = NSLock()
    private var state: State = .idle { didSet { onStateChange?(state) } }
    private var answerWhenRinging = false
    private var incomingPending = false
    /// CallKit has activated the audio session (didActivate) and not yet
    /// deactivated it.
    private var sessionActive = false
    private var account: String? // the AOR line currently registered (or registering)
    private let acceptAnyCertificate: Bool
    /// Whether libre/baresip are up. Independent of `state`: a failed
    /// registration leaves the stack running (restarting it returns EALREADY).
    private var stackRunning = false

    /// Headless tests only: replace the microphone with another baresip
    /// audio source, e.g. "aufile,/path/tone.wav", so the phone transmits
    /// where no capture device exists (simctl spawn). Set before start.
    public var audioSourceOverride: String?

    public init(acceptAnyCertificate: Bool = true) {
        self.acceptAnyCertificate = acceptAnyCertificate
    }

    deinit { cb_stop() }

    // MARK: CallEngine

    public func register(user: String, sip: SIPTarget) {
        if !stackRunning {
            start()
            if !stackRunning { return } // start() logged the reason
        }
        let aor = Self.aor(user: user, sip: sip)
        let already: Bool = lock.withLock { account == aor }
        if already, state == .ringing || state == .inCall {
            return // a call is up on this account; leave its flow alone
        }
        if already, state == .registered || state == .registering {
            // Same account, but the caller has a reason to doubt the binding:
            // a new gateway session (server restarted → registry empty) or a
            // wake with no INVITE pending (server has no usable route).
            // Our TLS flow may be dead without baresip having noticed yet,
            // so send a fresh REGISTER now rather than trust the state.
            let rc = cb_ua_register()
            if rc == 0 {
                state = .registering
                log("engine: re-registering \(user) (fresh REGISTER)")
            } else {
                log("engine: re-register failed (\(rc)); re-creating the user agent")
                lock.withLock { account = nil }
                register(user: user, sip: sip)
            }
            return
        }
        lock.withLock { account = aor }
        log("engine: registering \(user) via \(sip.host):\(sip.port)/tls")
        state = .registering
        let rc = cb_ua_alloc(aor)
        if rc != 0 {
            lock.withLock { account = nil }
            state = .failed("ua_alloc \(rc)")
            log("engine: ua_alloc failed (\(rc))")
        }
    }

    public func prepareForIncomingCall(callID: String, user: String, sip: SIPTarget) {
        lock.withLock { answerWhenRinging = true }
        let pending: Bool = lock.withLock { incomingPending }
        if pending {
            answerPending()
        } else {
            // The server woke us instead of dialling: whatever this engine
            // believes, the server has no usable registration. Register
            // (afresh if need be); the server dials once it sees it.
            register(user: user, sip: sip)
            log("engine: will answer call \(callID) when its INVITE arrives")
        }
    }

    public func hangup(callID: String) {
        cb_hangup()
        lock.withLock { answerWhenRinging = false; incomingPending = false }
        if state == .inCall || state == .ringing { state = cb_registered() ? .registered : .idle }
        log("engine: hangup \(callID)")
    }

    /// CallKit activated the audio session. baresip creates its AudioUnit
    /// player/source when the call is established, which is not tied to
    /// this moment in any way; units created before the session is live
    /// (or in the same instant as its activation) never render. So the
    /// devices are bound once BOTH the call and the session are up, on
    /// whichever of the two events comes last — the same shape as
    /// pjsip's set_snd_dev / linphone's activateAudioSession in didActivate.
    /// Manual audio (the contract WebRTC calls `useManualAudio`): the
    /// audiounit driver is held from stack start, so units it allocates
    /// when a call is established are initialised but not started. CallKit's
    /// didActivate releases the hold (units start), didDeactivate re-holds
    /// (units stop). The driver never starts audio on its own.
    public func audioSessionActivated() {
        guard stackRunning else { return }
        lock.withLock { sessionActive = true }
        cb_audio_interrupt(false)
        log("engine: audio session active → audio released (units start)")
        verifyAudioFlow()
    }

    /// Self-check: frames must flow shortly after audio is released during a
    /// call. Makes a device log conclusive on its own — "AUDIO STALLED"
    /// means CoreAudio delivered nothing, whatever else the log says.
    /// Every verdict of the current call, for headless tests (sim-call).
    public private(set) var audioVerdicts: [AudioVerdict] = []
    public var lastAudioVerdict: AudioVerdict? { audioVerdicts.last }
    public func clearAudioVerdicts() { lock.withLock { audioVerdicts.removeAll() } }

    public struct AudioVerdict: Equatable, Sendable {
        public var seconds: Int
        public var playFrames: UInt64
        public var recFrames: UInt64
        public var playEnergy: UInt64
        public var rtpTx: UInt32
        public var rtpRx: UInt32
        public var rxLost: Int32
        public var jitterMs: UInt32
        public var jbLate: UInt32
        public var jbUnderflow: UInt32
        public var verdict: String
        /// Tone (or speech) is actually being rendered: average |sample|
        /// over the window is well above silence/noise floor.
        public var audible: Bool { playFrames > 0 && playEnergy / max(playFrames, 1) > 200 }
    }

    /// Set once the checks for the current call are scheduled (both the
    /// activation and the establishment paths call in; whichever is last
    /// wins, the other is a no-op).
    private var flowCheckScheduled = false

    private func verifyAudioFlow() {
        let already: Bool = lock.withLock {
            let a = flowCheckScheduled
            flowCheckScheduled = true
            if !a { audioVerdicts.removeAll() } // verdicts are per call
            return a
        }
        if already { return }
        var p0: UInt64 = 0, r0: UInt64 = 0, e0: UInt64 = 0
        cb_audio_stats(&p0, &r0, &e0)
        var lastRx: UInt32 = 0 // RTP received as of the previous window
        for delay in [2.0, 5.0] {
            DispatchQueue.global().asyncAfter(deadline: .now() + delay) { [weak self] in
                guard let self, self.state == .inCall else { return }
                var p1: UInt64 = 0, r1: UInt64 = 0, e1: UInt64 = 0
                cb_audio_stats(&p1, &r1, &e1)
                var m = cb_media_stats_t()
                cb_media_stats(&m)
                let play = p1 - p0, rec = r1 - r0, energy = e1 - e0
                let rxSinceLast = m.rx_packets &- lastRx
                lastRx = m.rx_packets
                // Units: play = output rendered, rec = mic captured.
                // RTP: tx = packets sent to the server, rx = received from it
                // (cumulative; a count that stops rising is a stalled relay).
                let verdict: String
                if play == 0 && rec == 0 { verdict = "UNITS DEAD (no CoreAudio callbacks)" }
                else if play == 0 { verdict = "OUTPUT UNIT DEAD" }
                else if rec == 0 { verdict = "MIC DEAD (no capture)" }
                else if m.rx_packets == 0 { verdict = "NO RTP FROM SERVER (media path, not audio)" }
                else if rxSinceLast == 0 { verdict = "RTP FROM SERVER STOPPED (relay sent \(m.rx_packets) packets then nothing)" }
                else if m.tx_packets == 0 { verdict = "NO RTP SENT (encoder/tx path)" }
                else if energy / max(play, 1) <= 200 { verdict = "RENDERING SILENCE (RTP arrives, decoded audio is empty)" }
                else if m.rx_lost > 0 || m.jb_late > 0 || m.jb_underflow > 0 { verdict = "audio flowing but LOSSY (gaps you can hear)" }
                else { verdict = "audio flowing" }
                let v = AudioVerdict(seconds: Int(delay), playFrames: play, recFrames: rec, playEnergy: energy,
                                     rtpTx: m.tx_packets, rtpRx: m.rx_packets, rxLost: m.rx_lost, jitterMs: m.rx_jitter_us / 1000,
                                     jbLate: m.jb_late, jbUnderflow: m.jb_underflow, verdict: verdict)
                self.lock.withLock { self.audioVerdicts.append(v) }
                self.log("engine: \(verdict) — \(Int(delay))s: play=\(play) rec=\(rec) frames, energy/frame=\(energy / max(play, 1)); rtp tx=\(m.tx_packets) rx=\(m.rx_packets) err=\(m.rx_errors) lost=\(m.rx_lost) jitter=\(m.rx_jitter_us / 1000)ms; jbuf late=\(m.jb_late) lost=\(m.jb_lost) underflow=\(m.jb_underflow) overflow=\(m.jb_overflow)")
            }
        }
    }

    public func audioSessionDeactivated() {
        guard stackRunning else { return }
        lock.withLock { sessionActive = false }
        cb_audio_interrupt(true)
        log("engine: audio session inactive → audio held (units stopped)")
    }

    /// Tear the stack down (e.g. app going to background with no call).
    public func stop() {
        cb_ua_free()
        cb_stop()
        lock.withLock { account = nil; answerWhenRinging = false; incomingPending = false }
        stackRunning = false
        state = .idle
    }

    // MARK: internals

    static func aor(user: String, sip: SIPTarget) -> String {
        let parts = user.split(separator: "@", maxSplits: 1)
        let userPart = String(parts[0])
        let domain = parts.count > 1 ? String(parts[1]) : sip.host
        return "<sip:\(userPart)@\(domain);transport=tls>;outbound=\"sip:\(sip.host):\(sip.port);transport=tls\";regint=300;answermode=manual;audio_codecs=opus/48000/2,PCMU/8000/1"
    }

    private func answerPending() {
        let rc = cb_answer()
        lock.withLock { answerWhenRinging = false }
        log(rc == 0 ? "engine: answered" : "engine: answer failed (\(rc))")
    }

    private func start() {
        state = .starting
        let config = """
        # generated by BaresipCallEngine
        sip_listen              0.0.0.0:0
        sip_verify_server       \(acceptAnyCertificate ? "no" : "yes")
        call_local_timeout      60
        call_max_calls          1
        audio_player            audiounit,default
        audio_source            \(audioSourceOverride ?? "audiounit,default")
        audio_alert             none
        audio_srate             48000
        audio_channels          1
        module                  audiounit.so
        module                  aufile.so
        module                  opus.so
        module                  g711.so
        module                  ice.so
        module                  srtp.so
        module                  auconv.so
        module                  auresamp.so
        """
        let ctx = Unmanaged.passUnretained(self).toOpaque()
        let rc = cb_start(config, { ctx, event, peer, text in
            guard let ctx else { return }
            let engine = Unmanaged<BaresipCallEngine>.fromOpaque(ctx).takeUnretainedValue()
            engine.handle(event: event, peer: peer.map { String(cString: $0) } ?? "", text: text.map { String(cString: $0) } ?? "")
        }, ctx)
        if rc != 0 {
            state = .failed("start \(rc)")
            log("engine: start failed (\(rc))")
        } else {
            stackRunning = true
            // Manual audio: hold the driver until CallKit activates a session.
            cb_audio_interrupt(true)
            log("engine: started \(String(cString: cb_version())); audio held until CallKit activates")
        }
    }

    /// Runs on the libre loop thread.
    private func handle(event: cb_event_t, peer: String, text: String) {
        switch event {
        case CB_EVENT_REGISTER_OK:
            state = .registered
            log("engine: registered")
        case CB_EVENT_REGISTER_FAIL:
            lock.withLock { account = nil } // allow a fresh attempt
            state = .failed("register: \(text)")
            log("engine: registration failed: \(text)")
        case CB_EVENT_CALL_INCOMING:
            state = .ringing
            log("engine: INVITE from \(peer)")
            let answerNow: Bool = lock.withLock { incomingPending = true; return answerWhenRinging }
            if answerNow {
                answerPending()
            } else {
                onIncomingCall?(peer)
            }
        case CB_EVENT_CALL_RINGING:
            log("engine: ringing \(peer)")
        case CB_EVENT_CALL_ESTABLISHED:
            let active: Bool = lock.withLock { incomingPending = false; return sessionActive }
            state = .inCall
            log("engine: call established with \(peer); audio \(active ? "starting (session already active)" : "held until didActivate")")
            if active { verifyAudioFlow() }
        case CB_EVENT_CALL_CLOSED:
            log("engine: call closed (\(text))")
            lock.withLock { incomingPending = false; answerWhenRinging = false; flowCheckScheduled = false }
            // sessionActive is cleared by didDeactivate, which CallKit sends
            // after the call is reported ended.
            state = cb_registered() ? .registered : .idle
            onCallEnded?(text)
        case CB_EVENT_LOG:
            logger.info("baresip: \(text, privacy: .public)")
            if text.contains("register") || text.contains("tls") || text.contains("dns") || text.contains("fail") || text.contains("error") || text.contains("audiounit") || text.contains("rtp") || text.contains("stream:") || text.contains("rtcp") {
                log("baresip: \(text)")
            }
        default:
            if !text.isEmpty { log("engine event: \(text)") }
        }
    }
}
