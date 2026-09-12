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
        case idle, starting, registering, registered, ringing, dialing, inCall, failed(String)
    }

    public var onIncomingCall: ((String, String?) -> Void)?
    public var onCallEnded: ((String) -> Void)?
    public var onOutgoingRinging: (() -> Void)?
    public var onCallEstablished: (() -> Void)?
    public var onTransferFailed: ((String) -> Void)?
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
    private var domain = ""      // SIP domain of the account, completes bare dial targets
    /// What `register` was last asked for, so a failed registration can be
    /// retried on our own initiative (a dead connection after the app was
    /// suspended) rather than waiting for the next gateway welcome.
    private var lastRegistration: (user: String, sip: SIPTarget)?
    private var registerRetries = 0
    private var retryGeneration = 0 // bumped to cancel a pending retry
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

    /// SIP Digest credential for the app leg: the device id and enrolment
    /// token. Goes into the account as auth_user / auth_pass so baresip
    /// answers the server's 401 on REGISTER and INVITE by itself.
    private var credentials: (user: String, pass: String)?

    public func setCredentials(username: String, password: String) {
        lock.withLock { credentials = (username, password) }
    }

    deinit { cb_stop() }

    // MARK: CallEngine

    public func register(user: String, sip: SIPTarget) {
        guard ensureStack() else { return } // start()/stop() logged the reason
        lock.withLock {
            lastRegistration = (user, sip)
            retryGeneration += 1 // an explicit register supersedes a pending retry
        }
        let aor = self.aor(user: user, sip: sip)
        lock.withLock { domain = user.split(separator: "@", maxSplits: 1).count > 1 ? String(user.split(separator: "@", maxSplits: 1)[1]) : sip.host }
        let already: Bool = lock.withLock { account == aor }
        if already, state == .ringing || state == .inCall || state == .dialing {
            return // a call is up on this account; leave its flow alone
        }
        if already, state == .registered || state == .registering {
            // Same account, but the caller has a reason to doubt the binding:
            // a new gateway session (server restarted → registry empty) or a
            // wake with no INVITE pending (server has no usable route).
            // Our TLS flow may be dead without baresip having noticed yet,
            // so drop the cached connections (a send on a dead one fails at
            // once with EPROTO) and send a fresh REGISTER on a new one.
            _ = cb_reset_transports()
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
            // rc is -errno straight from the SIP stack: a synchronous
            // connect() failure on the TLS socket to the server comes back
            // this way (libre returns the errno of connect() up through
            // sipreg → ua_register), so name it.
            let why = String(cString: strerror(-rc))
            state = .failed("ua_alloc \(rc): \(why)")
            log("engine: ua_alloc failed (\(rc): \(why)); the REGISTER's connection to \(sip.host):\(sip.port) could not be opened")
            scheduleRegisterRetry() // e.g. the loop was busy/recovering: try again
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

    public func dial(callID: String, to target: String) {
        guard stackRunning else { log("engine: cannot dial \(target): stack not running"); return }
        let uri = Self.dialURI(target, domain: lock.withLock { domain })
        state = .dialing
        log("engine: dialling \(uri) for \(callID)")
        let rc = cb_dial(uri)
        if rc != 0 {
            log("engine: dial failed (\(rc))")
            state = cb_registered() ? .registered : .idle
            onCallEnded?("dial failed (\(rc))")
        }
    }

    /// "202" → "sip:202@dialler"; "202@dialler" → "sip:202@dialler";
    /// "sip:…" unchanged. Digits typed on a keypad take the same route.
    static func dialURI(_ target: String, domain: String) -> String {
        let t = target.trimmingCharacters(in: .whitespacesAndNewlines)
        if t.hasPrefix("sip:") || t.hasPrefix("sips:") { return t }
        if t.contains("@") { return "sip:\(t)" }
        return "sip:\(t)@\(domain)"
    }

    public func setMuted(_ muted: Bool) {
        guard stackRunning else { return }
        cb_mute(muted)
        log("engine: microphone \(muted ? "muted" : "unmuted")")
    }

    /// Hold: baresip re-INVITEs with the audio stream sendonly and stops its
    /// audio; resume re-INVITEs sendrecv. The audio units stay bound to the
    /// CallKit session throughout (CallKit keeps it active for a held call).
    public func setHeld(_ held: Bool) {
        guard stackRunning else { return }
        let rc = cb_hold(held)
        log(rc == 0 ? "engine: \(held ? "held" : "resumed")" : "engine: \(held ? "hold" : "resume") failed (\(rc))")
    }

    /// Blind transfer: baresip sends REFER with the target URI; the server
    /// answers 202, connects the other party, and NOTIFYs the outcome —
    /// 200 ends this call ("Call transfered"), a failure keeps it up.
    public func transfer(callID: String, to target: String) {
        guard stackRunning else { return }
        let uri = Self.dialURI(target, domain: lock.withLock { domain })
        let rc = cb_transfer(uri)
        log(rc == 0 ? "engine: REFER \(callID) → \(uri)" : "engine: transfer failed to send (\(rc))")
        if rc != 0 { onTransferFailed?("could not send REFER (\(rc))") }
    }

    public func hangup(callID: String) {
        cb_hangup()
        lock.withLock { answerWhenRinging = false; incomingPending = false }
        if state == .inCall || state == .ringing || state == .dialing { state = cb_registered() ? .registered : .idle }
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
        // Windows at 2 s and 5 s, then every 5 s for the life of the call
        // (capped), so a call that degrades at 30 s is visible; each window
        // reports the audio it rendered since the previous one.
        let checkpoints: [Double] = [2.0, 5.0] + stride(from: 10.0, through: 300.0, by: 5.0).map { $0 }
        for delay in checkpoints {
            DispatchQueue.global().asyncAfter(deadline: .now() + delay) { [weak self] in
                guard let self, self.state == .inCall else { return }
                var p1: UInt64 = 0, r1: UInt64 = 0, e1: UInt64 = 0
                cb_audio_stats(&p1, &r1, &e1)
                var m = cb_media_stats_t()
                cb_media_stats(&m)
                let play = p1 - p0, rec = r1 - r0, energy = e1 - e0
                (p0, r0, e0) = (p1, r1, e1)
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
                self.lock.withLock {
                    self.audioVerdicts.append(v)
                    if self.audioVerdicts.count > 60 { self.audioVerdicts.removeFirst() }
                }
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
        lock.withLock {
            account = nil; answerWhenRinging = false; incomingPending = false
            lastRegistration = nil; retryGeneration += 1
        }
        stackRunning = false
        state = .idle
    }

    /// The gateway session came back after a drop. iOS killed both sockets
    /// in the same suspension and baresip only learns its one is dead on
    /// the next send, so drop the registration (and its connection) now and
    /// let the welcome's `register` start a fresh one. Left alone during a
    /// call.
    public func resetRegistration() {
        switch state {
        case .ringing, .dialing, .inCall:
            log("engine: registration reset deferred: a call is in progress")
            return
        default:
            break
        }
        if stackRunning && !cb_alive() {
            // The loop thread died with the connection; a fresh stack is
            // the only way back, and the welcome's register starts it.
            log("engine: SIP loop is dead; tearing the stack down for a fresh start")
            stop()
            return
        }
        lock.withLock {
            account = nil
            registerRetries = 0
            retryGeneration += 1
        }
        cb_ua_free()
        // iOS tore the SIP socket down with the gateway's; libre still has
        // it cached and would send the next REGISTER on it (EPROTO, seen
        // as "ua_alloc -100" after every unlock). Rebuild the transports.
        _ = cb_reset_transports()
        if stackRunning { state = .idle }
        log("engine: registration dropped (session came back); transports reset; registering afresh")
    }

    /// The stack must be running with a live loop thread. libre ends its
    /// loop on a poll error (seen on Darwin after the SIP connection was
    /// reset by the peer); a dead stack is torn down and started again.
    @discardableResult
    private func ensureStack() -> Bool {
        if stackRunning && !cb_alive() {
            log("engine: SIP loop died; restarting the stack")
            stop()
        }
        if !stackRunning { start() }
        return stackRunning
    }

    /// A failed registration is retried by us with backoff (2, 4, 8, then
    /// 15 s) on the last requested account, re-creating the user agent so a
    /// dead connection is replaced. Cancelled by success, an explicit
    /// register, a reset, or stop.
    private func scheduleRegisterRetry() {
        let (gen, attempt, last): (Int, Int, (user: String, sip: SIPTarget)?) = lock.withLock {
            retryGeneration += 1
            registerRetries += 1
            return (retryGeneration, registerRetries, lastRegistration)
        }
        guard let last else { return }
        let delay = min(15.0, pow(2.0, Double(attempt)))
        log("engine: retrying registration in \(Int(delay))s (attempt \(attempt))")
        DispatchQueue.global().asyncAfter(deadline: .now() + delay) { [weak self] in
            guard let self, self.lock.withLock({ self.retryGeneration == gen }) else { return }
            // The failed attempt may have gone to a dead cached connection;
            // start the retry on fresh transports.
            if self.stackRunning { _ = cb_reset_transports() }
            self.register(user: last.user, sip: last.sip)
        }
    }

    // MARK: internals

    func aor(user: String, sip: SIPTarget) -> String {
        let parts = user.split(separator: "@", maxSplits: 1)
        let userPart = String(parts[0])
        let domain = parts.count > 1 ? String(parts[1]) : sip.host
        let auth: String = lock.withLock {
            guard let c = credentials else { return "" }
            return ";auth_user=\(c.user);auth_pass=\(c.pass)"
        }
        // audio_codecs names codecs by their AUDIO channel count, not the SDP
        // one: with `opus_stereo no` baresip registers Opus as opus/48000/1
        // (the SDP still says opus/48000/2). Listing /2 here silently fails
        // to find the codec and the call falls back to PCMU — caught by the
        // harness when the "Opus" gate turned out to be running G.711.
        // G.722 likewise: baresip names it G722/16000/1 (its audio rate);
        // the SDP says G722/8000 (RFC 3551's RTP clock). G722/8000/1 here
        // is "audio codec not found" and the call falls back to PCMU.
        return "<sip:\(userPart)@\(domain);transport=tls>\(auth);outbound=\"sip:\(sip.host):\(sip.port);transport=tls\";regint=300;answermode=manual;audio_codecs=opus/48000/1,G722/16000/1,PCMU/8000/1"
    }

    private func answerPending() {
        let rc = cb_answer()
        lock.withLock { answerWhenRinging = false }
        log(rc == 0 ? "engine: answered" : "engine: answer failed (\(rc))")
    }

    /// The baresip configuration the stack starts with. A function of its
    /// two inputs so the audio profile is unit-testable (SPEC §6 near-term
    /// item 5): every key here was chosen against a measurement, see the
    /// comments, and the harness phone (`harness/baresip/config`) mirrors
    /// the same profile so headless gates measure what the app runs.
    static func stackConfig(acceptAnyCertificate: Bool, audioSource: String?) -> String {
        """
        # generated by BaresipCallEngine
        sip_listen              0.0.0.0:0
        sip_verify_server       \(acceptAnyCertificate ? "no" : "yes")
        call_local_timeout      60
        call_max_calls          1
        audio_player            audiounit,default
        audio_source            \(audioSource ?? "audiounit,default")
        audio_alert             none

        # Playout: an adaptive jitter buffer of 2–8 frames (40–160 ms at
        # 20 ms) instead of the fixed default 5–10 (100–200 ms), which was
        # most of the mouth-to-ear budget; the play buffer adapts likewise,
        # with a 40 ms floor: after a lost packet the jitter buffer withholds
        # the next one until it has refilled, and one extra frame of play
        # buffer rides that out (measured: no delay cost, fewer gaps).
        audio_jitter_buffer_type  adaptive
        audio_jitter_buffer_delay 2-8
        audio_buffer              40-160
        audio_buffer_mode         adaptive

        # Opus for speech on Wi-Fi: voip mode, in-band FEC so a single lost
        # packet is recovered from the next, 32 kbit/s mono, complexity 6
        # (10, the default, is CPU for nothing audible). No DTX: it fights
        # the voice-processing unit's comfort noise and hides silent legs.
        # opus_packet_loss is what makes FEC real: baresip's module only
        # tells the encoder to add redundancy, and only decodes it, when an
        # expected loss is set (modules/opus/decode.c: fec = packet_loss > 0).
        # Measured: with it 0 gaps at 2 % loss, without it 4 (SPEC §7.2).
        opus_application        voip
        opus_inbandfec          yes
        opus_packet_loss        10
        opus_bitrate            32000
        opus_stereo             no
        opus_sprop_stereo       no
        opus_complexity         6
        opus_cbr                no
        opus_dtx                no

        # RTP: statistics on (the loss/jitter numbers the self-check reads
        # are zero without them), dead media hangs the call up after 30 s,
        # EF marking on media (the transport patch maps it to WMM voice).
        rtp_stats               yes
        rtp_timeout             30
        rtp_tos                 184

        module                  audiounit.so
        module                  aufile.so
        module                  opus.so
        module                  g722.so
        module                  g711.so
        module                  ice.so
        module                  srtp.so
        module                  auconv.so
        module                  auresamp.so
        """
    }

    private func start() {
        state = .starting
        let config = Self.stackConfig(acceptAnyCertificate: acceptAnyCertificate, audioSource: audioSourceOverride)
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
            lock.withLock {
                registerRetries = 0
                retryGeneration += 1 // cancel any pending retry
            }
            state = .registered
            log("engine: registered")
        case CB_EVENT_REGISTER_FAIL:
            lock.withLock { account = nil } // allow a fresh attempt
            state = .failed("register: \(text)")
            log("engine: registration failed: \(text)")
            scheduleRegisterRetry()
        case CB_EVENT_CALL_INCOMING:
            state = .ringing
            // For this event the shim passes the caller's From display name
            // as `text` ("" when the caller sent none).
            let displayName = text.isEmpty ? nil : text
            log("engine: INVITE from \(peer)\(displayName.map { " (\"\($0)\")" } ?? "")")
            let answerNow: Bool = lock.withLock { incomingPending = true; return answerWhenRinging }
            if answerNow {
                answerPending()
            } else {
                onIncomingCall?(peer, displayName)
            }
        case CB_EVENT_CALL_OUTGOING:
            state = .dialing
            log("engine: INVITE sent to \(peer)")
        case CB_EVENT_CALL_TRANSFER_FAILED:
            log("engine: transfer failed: \(text)")
            onTransferFailed?(text)
        case CB_EVENT_CALL_RINGING, CB_EVENT_CALL_PROGRESS:
            log("engine: far end \(event == CB_EVENT_CALL_RINGING ? "ringing" : "progress") \(peer)")
            onOutgoingRinging?()
        case CB_EVENT_CALL_ESTABLISHED:
            let active: Bool = lock.withLock { incomingPending = false; return sessionActive }
            let wasOutgoing = state == .dialing
            state = .inCall
            log("engine: call established with \(peer); audio \(active ? "starting (session already active)" : "held until didActivate")")
            if active { verifyAudioFlow() }
            if wasOutgoing { onCallEstablished?() }
        case CB_EVENT_CALL_CLOSED:
            log("engine: call closed (\(text))")
            lock.withLock { incomingPending = false; answerWhenRinging = false; flowCheckScheduled = false }
            // sessionActive is cleared by didDeactivate, which CallKit sends
            // after the call is reported ended.
            state = cb_registered() ? .registered : .idle
            onCallEnded?(text)
        case CB_EVENT_LOG:
            logger.info("baresip: \(text, privacy: .public)")
            if text.contains("cbaresip") || text.contains("register") || text.contains("tls") || text.contains("dns") || text.contains("fail") || text.contains("error") || text.contains("audiounit") || text.contains("rtp") || text.contains("stream:") || text.contains("rtcp") {
                log("baresip: \(text)")
            }
        default:
            if !text.isEmpty { log("engine event: \(text)") }
        }
    }
}
