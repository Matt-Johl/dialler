import AVFoundation
import Combine
import DiallerCore
import DiallerProtocol
import Foundation
import MetricKit
// @preconcurrency: NetworkExtension's classes (NEAppPushManager) are not yet
// marked Sendable, and its completion handlers are @Sendable. We only touch
// the manager after hopping back to the main actor inside those handlers,
// so the capture is safe; this silences the false Sendable warnings.
@preconcurrency import NetworkExtension
import os
import UIKit
#if canImport(DiallerEngine)
import DiallerEngine
#endif

/// App-side wiring of DiallerCore: config, the foreground LAN transport,
/// the call controller behind CallKit, and the directory. UI-observable.
@MainActor
final class AppModel: ObservableObject {
    // Settings
    @Published var host = ""
    @Published var port = "7443"
    @Published var deviceID = ""
    @Published var token = ""
    @Published var acceptAnyCertificate = true

    // Status
    @Published private(set) var status = "disconnected"
    @Published private(set) var sessionID = ""
    @Published private(set) var contacts: [DirectoryContact] = []
    /// The Recents list (SPEC §6 item 6), newest first. Written by the
    /// controller's record of every call and by the extension's sidecars.
    @Published private(set) var recents: [CallRecord] = []
    /// Missed calls the user has not looked at yet: the Recents tab's badge.
    @Published private(set) var unseenMissed = 0
    @Published private(set) var log: [String] = []
    @Published private(set) var localPushStatus = "not configured"
    /// Whether iOS is actually running the provider extension right now.
    /// This is the one that decides whether a call can reach the phone
    /// while the app is not open: with it false the server's wake has
    /// nowhere to land and the caller gets "callee offline, wake
    /// undeliverable". It had been visible only as a log line, which cost
    /// a morning's debugging to work out (2026-09-16).
    @Published private(set) var backgroundCalls = "unknown"
    /// The SSIDs of the saved Local Push configuration, as loaded from
    /// the framework's preferences (the source of truth; the app persists
    /// nothing of its own). Settings prefills its field from this.
    @Published private(set) var localPushSSIDs: [String] = []

    /// The call in progress, for the in-call screen. Nil while idle or
    /// merely ringing (ringing is CallKit's UI alone).
    struct ActiveCall: Identifiable, Equatable {
        var id: String
        var title: String
        var outgoing: Bool
        var connectedAt: Date?
        var muted = false
        var speaker = false
        var held = false
        /// Outgoing only: what the call is doing, from the SIP the
        /// controller sees. Stays on screen through the failure tone.
        var progress: CallProgress = .calling
        var status: String {
            if connectedAt == nil {
                return outgoing ? progress.label : "Connecting…"
            }
            return held ? "On hold" : "Connected"
        }
    }
    /// Every call the in-call screen shows (answered, or outgoing and
    /// connecting), oldest first. With call waiting there are up to two:
    /// one active and one held.
    @Published private(set) var calls: [ActiveCall] = []
    /// The call the screen is about: the one not on hold, else (all held)
    /// the first, so the screen stays up for a single held call.
    var activeCall: ActiveCall? { calls.first { !$0.held } ?? calls.first }
    /// The other call, on hold, while two are up — named under the active
    /// party on the in-call screen (switching is the system's control).
    var heldCall: ActiveCall? {
        guard let active = activeCall else { return nil }
        return calls.first { $0.id != active.id && $0.held }
    }

    /// Call waiting (Settings): a second incoming call rings over the
    /// current one (Hold & Accept / End & Accept / Decline); off, a second
    /// caller hears busy at once. Persisted; default on.
    @Published var callWaiting: Bool {
        didSet {
            controller.callWaitingEnabled = callWaiting
            UserDefaults.standard.set(callWaiting, forKey: "callWaiting")
        }
    }

    private func upsertCall(_ id: String, outgoing: Bool, connectedAt: Date?) {
        let title = controller.activeCalls.first { $0.wake.callID == id }.map { self.title(for: $0) } ?? "Call"
        if let i = calls.firstIndex(where: { $0.id == id }) {
            calls[i].connectedAt = connectedAt ?? calls[i].connectedAt
            calls[i].title = title
        } else {
            var c = ActiveCall(id: id, title: title, outgoing: outgoing, connectedAt: connectedAt)
            c.muted = calls.first?.muted ?? false // one microphone
            c.speaker = calls.first?.speaker ?? false // one route
            calls.append(c)
        }
    }

    /// A short-lived line on the in-call screen for something that happened
    /// to the call without changing its state — a refused transfer, where
    /// the call simply continues and nothing else would say so.
    @Published private(set) var notice: String?
    private var noticeTask: Task<Void, Never>?

    private func show(notice text: String) {
        notice = text
        noticeTask?.cancel()
        noticeTask = Task { @MainActor [weak self] in
            try? await Task.sleep(nanoseconds: 6_000_000_000)
            guard !Task.isCancelled else { return }
            self?.notice = nil
        }
    }

    private func clearNotice() {
        noticeTask?.cancel()
        noticeTask = nil
        notice = nil
    }

    private let store: AppConfigStore = AppGroupConfigStore(appGroup: DiallerIDs.appGroup)
    private let callKit = CallKitBridge()
    /// Call-progress tones into the call's audio session (plan Phase J).
    private lazy var tones = TonePlayer(log: { [weak self] m in Task { @MainActor in self?.append(m) } })
    private let engine: CallEngine
    @Published private(set) var engineState = "no engine"
    private lazy var controller = CallController(ui: callKit, engine: engine, log: { [weak self] m in
        Task { @MainActor in self?.append(m) }
    })
    /// The gateway session, kept up by `GatewaySession` (reconnects after a
    /// drop while we are in the foreground).
    private var session: GatewaySession?
    /// A session was lost since the last welcome: the SIP registration died
    /// with it and must be rebuilt when the next welcome arrives.
    private var sessionDropped = false
    private var eventTask: Task<Void, Never>?
    /// The saved Local Push configuration, kept loaded so its delegate stays
    /// attached: iOS hands incoming calls to that delegate whenever this
    /// process exists (foreground or suspended) and only goes through
    /// PushKit to launch a dead one. See `LocalPushDelegate`.
    private var pushManager: NEAppPushManager?
    /// Every loaded manager instance, kept alive so their delegates stay set.
    private var pushManagers: [NEAppPushManager] = []
    private lazy var localPushDelegate = LocalPushDelegate(model: self)
    private var book = AddressBook()
    /// Thread-safe caller-name lookup for incoming calls (the controller
    /// resolves names off the main actor). Kept in step with `contacts`.
    private nonisolated let nameIndex = DirectoryNameIndex()
    private let recentsStore = RecentsStore()
    /// When the user last opened the Recents tab; missed calls after it
    /// count towards the badge. A per-device convenience, so UserDefaults.
    private var recentsSeenAt: Date {
        get { UserDefaults.standard.object(forKey: "recentsSeenAt") as? Date ?? .distantPast }
        set { UserDefaults.standard.set(newValue, forKey: "recentsSeenAt") }
    }
    private let logger = Logger(subsystem: DiallerIDs.bundlePrefix, category: "app")
    /// Persistent log in the App Group (survives a kill; uploaded to the dev
    /// server on the next launch), beside the in-memory view.
    private let fileLog = Breadcrumb.log
    private let diagnostics = DiagnosticsCollector()
    @Published private(set) var diagnosticsStatus = ""

    init() {
        callWaiting = UserDefaults.standard.object(forKey: "callWaiting") as? Bool ?? true
        Breadcrumb.drop("AppModel: creating the SIP engine")
        #if canImport(DiallerEngine)
        let baresip = BaresipCallEngine(acceptAnyCertificate: true)
        engine = baresip
        engineState = "baresip idle"
        #else
        let logging = LoggingCallEngine()
        engine = logging
        #endif
        if let cfg = store.load() {
            host = cfg.gateway.host
            port = String(cfg.gateway.port)
            deviceID = cfg.deviceID
            token = cfg.token
            acceptAnyCertificate = cfg.gateway.acceptAnyCertificate
        }
        fileLog?.write("---- launch \(Bundle.main.infoDictionary?["CFBundleShortVersionString"] ?? "?") ----")
        // MetricKit hands the app its own crash / hang / CPU-kill reports on
        // the launch after they happened; they are queued and uploaded with
        // the logs, so a termination on the phone is readable on the Mac.
        diagnostics.onPayload = { [weak self] data in
            DiagnosticsClient.enqueue(kind: "metrickit", data: data)
            Task { @MainActor in self?.append("diagnostics: MetricKit report queued (\(data.count) bytes)") }
        }
        MXMetricManager.shared.add(diagnostics)
        Task { await sendDiagnostics(reason: "launch") }
        controller.callWaitingEnabled = callWaiting
        // Recents: every call the controller lets go of, whatever became of
        // it, plus what the extension noted while this process was not
        // running. Folded again on each return to the foreground.
        recents = recentsStore?.foldPending() ?? []
        refreshUnseenMissed()
        controller.onCallEnded = { [weak self] record in
            Task { @MainActor in self?.recordCall(record) }
        }
        // A second call rings without a ringtone (iOS suppresses it) and
        // without a tone of its own; the beep is ours to play, into the ear
        // of the person already on a call.
        callKit.onCallWaiting = { [weak self] waiting in
            Task { @MainActor in
                guard let self else { return }
                self.tones.want(waiting ? CallTones.callWaiting : nil)
            }
        }
        // Every other call-progress tone (ring-back, busy, congestion) is
        // chosen by the controller, which knows the SIP, and played here,
        // which owns the audio session's player.
        // What the screen says under the name: "Ringing…" once the far end
        // is genuinely alerting, and why a call failed for as long as its
        // tone plays — it used to read "Calling…" through both.
        callKit.onProgress = { [weak self] id, progress in
            Task { @MainActor in
                guard let self, let i = self.calls.firstIndex(where: { $0.id == id }) else { return }
                self.calls[i].progress = progress
            }
        }
        callKit.onTone = { [weak self] tone in
            Task { @MainActor in
                guard let self else { return }
                self.tones.want(tone)
            }
        }
        callKit.onAnswer = { [weak self] id in
            guard let self else { return }
            // Answering a second call holds the first (the controller tells
            // the engine; CallKit's Hold & Accept sends the hold action too).
            self.controller.userAnswered(callID: id)
            for i in self.calls.indices where self.calls[i].id != id { self.calls[i].held = true }
            self.upsertCall(id, outgoing: false, connectedAt: Date())
        }
        callKit.onEnd = { [weak self] id in
            guard let self else { return }
            self.controller.userEnded(callID: id)
            self.callEnded(id)
        }
        callKit.onStart = { [weak self] id in
            guard let self else { return }
            self.clearNotice()
            self.controller.userStarted(callID: id)
            self.upsertCall(id, outgoing: true, connectedAt: nil)
        }
        callKit.onEnded = { [weak self] id in self?.callEnded(id) }
        callKit.onStartFailed = { [weak self] id in
            self?.controller.startFailed(callID: id)
            self?.calls.removeAll { $0.id == id }
        }
        callKit.onConnected = { [weak self] id in
            guard let self, let i = self.calls.firstIndex(where: { $0.id == id }) else { return }
            self.calls[i].connectedAt = Date()
        }
        callKit.onMute = { [weak self] id, muted in
            guard let self else { return }
            self.controller.setMuted(muted)
            for i in self.calls.indices { self.calls[i].muted = muted } // one microphone
        }
        controller.onTransferFailed = { [weak self] reason, progress in
            Task { @MainActor in
                guard let self else { return }
                self.append("transfer refused: \(reason)")
                // On screen, not only in the log: the call carries on, so
                // without this the user taps Transfer and nothing visible
                // happens at all.
                self.show(notice: progress.map { "Transfer failed — \($0.label)" } ?? "Transfer failed")
            }
        }
        // Show the directory's friendly name for a known incoming caller
        // (e.g. "SIP phone (101)" instead of sip:101@…). The controller runs
        // this off the main actor, so read a snapshot of the contacts.
        controller.resolveDisplayName = { [weak self] uri, provided in
            self?.nameIndex.name(forURI: uri) ?? provided
        }
        callKit.onHold = { [weak self] id, held in
            guard let self else { return }
            self.controller.setHeld(callID: id, held)
            if let i = self.calls.firstIndex(where: { $0.id == id }) { self.calls[i].held = held }
        }
        callKit.onAudioActivated = { [weak self] in
            self?.engine.audioSessionActivated()
            // After the engine, never before: a tone must not be what
            // activates the session, and must not race the call's own
            // audio units into it (see `TonePlayer`).
            Task { @MainActor in self?.tones.session(active: true) }
        }
        callKit.onAudioDeactivated = { [weak self] in
            self?.engine.audioSessionDeactivated()
            Task { @MainActor in self?.tones.session(active: false) }
        }
        callKit.onLog = { [weak self] m in Task { @MainActor in self?.append(m) } }
        // iOS suspends the app (and kills its sockets) in the background;
        // reconnect the moment we are back, and do not retry while away.
        let nc = NotificationCenter.default
        nc.addObserver(forName: UIApplication.willResignActiveNotification, object: nil, queue: .main) { [weak self] _ in
            // Leaving the foreground: a Local Push delivery to this
            // (soon suspended) process must find a delegate on the loaded
            // manager. Re-attach now; cheap, and it is exactly when it counts.
            Task { @MainActor in self?.attachLocalPushDelegate() }
        }
        nc.addObserver(forName: UIApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.appBecameActive() }
        }
        nc.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.appEnteredBackground() }
        }
        #if canImport(DiallerEngine)
        baresip.log = { [weak self] m in Task { @MainActor in self?.append(m) } }
        baresip.onStateChange = { [weak self] s in Task { @MainActor in self?.engineState = "baresip \(s)" } }
        #else
        logging.log = { [weak self] m in Task { @MainActor in self?.append(m) } }
        #endif
    }

    // MARK: Calls

    /// Place a call. `target` is whatever the user gave: a directory URI
    /// ("sip:202@dialler"), a user ("202") or digits from the keypad; the
    /// engine completes bare targets with the account's domain and the
    /// server routes local users to apps and everything else to the trunk.
    func dial(_ target: String) {
        let t = target.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !t.isEmpty else { return }
        let name = contacts.first { $0.uri == t || CallController.userPart(of: $0.uri) == t }?.displayName
        if controller.startCall(to: t, displayName: name) == nil {
            append("call to \(t) not started (see log above)")
        }
    }

    func hangUp() {
        guard let id = activeCall?.id else { return }
        callKit.requestEnd(callID: id)
    }

    /// Swap the active and the held call: one CallKit transaction, hold the
    /// active and resume the held; the hold actions come back through
    /// `onHold` and the screen follows. On iOS 26 the system's own swap
    /// banner does this; the in-call screen offers it only on older iOS.
    func swapCalls() {
        guard let active = activeCall, let held = heldCall else { return }
        callKit.requestSwap(active: active.id, held: held.id)
    }

    func toggleMute() {
        guard let call = activeCall else { return }
        callKit.requestMute(callID: call.id, muted: !call.muted)
    }

    /// Blind transfer of the active call. The call ends when the server has
    /// connected the other party; a refusal is logged and the call stays up.
    func transfer(to target: String) {
        guard let call = activeCall, call.connectedAt != nil else { return }
        controller.transfer(callID: call.id, to: target)
    }

    func toggleHold() {
        guard let call = activeCall, call.connectedAt != nil else { return }
        callKit.requestHold(callID: call.id, held: !call.held)
    }

    func toggleSpeaker() {
        guard let call = activeCall else { return }
        let speaker = !call.speaker
        do {
            try AVAudioSession.sharedInstance().overrideOutputAudioPort(speaker ? .speaker : .none)
            for i in calls.indices { calls[i].speaker = speaker } // one route
        } catch {
            append("speaker toggle failed: \(error.localizedDescription)")
        }
    }

    /// CallKit ended a call that the in-call screen is showing (remote
    /// hangup, failure) — the bridge reports it through the controller's
    /// `end`, which calls this.
    func callEnded(_ id: String) {
        calls.removeAll { $0.id == id }
        if calls.isEmpty { clearNotice() } // it belonged to a call that is gone
        // The last call is over and we are not on screen: back to the
        // background rule (no session; the extension covers wakes).
        if controller.activeCalls.isEmpty, UIApplication.shared.applicationState != .active {
            session?.setActive(false)
        }
    }

    /// A call the extension woke rings while the app is in the background,
    /// where the keeper holds no session of its own — so the caller's
    /// hangup, a wake_cancel the server sends on the app's socket (and
    /// replays to a session that connects later), had nowhere to arrive and
    /// the phone rang until the user gave up (2026-09-13, repeatedly). CallKit
    /// keeps the process running while a call is tracked, so hold a session
    /// for exactly that long; `callEnded` lets it go again.
    private func holdSessionWhileCallTracked() {
        session?.setActive(true)
    }

    // MARK: Recents

    private func recordCall(_ record: CallRecord) {
        guard let store = recentsStore else { return }
        store.record(record)
        recents = store.foldPending() // the extension may have noted this wake too
        refreshUnseenMissed()
    }

    /// The name to show for a record: the directory's current name for the
    /// number, else the name known when the call ended, else the number.
    func recentName(for record: CallRecord) -> String {
        nameIndex.name(forURI: record.counterpart.uri)
            ?? (record.counterpart.displayName?.isEmpty == false ? record.counterpart.displayName! : record.number)
    }

    func deleteRecent(id: String) {
        guard let store = recentsStore else { return }
        recents = store.delete(id: id)
        refreshUnseenMissed()
    }

    func clearRecents() {
        recentsStore?.clear()
        recents = []
        refreshUnseenMissed()
    }

    /// The user is looking at the list: nothing on it is unseen any more.
    func markRecentsSeen() {
        recentsSeenAt = Date()
        refreshUnseenMissed()
    }

    private func refreshUnseenMissed() {
        let since = recentsSeenAt
        unseenMissed = recents.filter { $0.isMissed && $0.endedAt > since }.count
    }

    private func title(for call: TrackedCall) -> String {
        if call.direction == .outgoing {
            return call.wake.to.displayName ?? call.target
        }
        let provided = call.wake.from.displayName?.isEmpty == false ? call.wake.from.displayName : nil
        return nameIndex.name(forURI: call.wake.from.uri) ?? provided ?? CallController.numberPart(of: call.wake.from.uri)
    }

    var currentConfig: AppConfig {
        AppConfig(gateway: GatewayEndpoint(host: host, port: UInt16(port) ?? 7443, acceptAnyCertificate: acceptAnyCertificate),
                  deviceID: deviceID, token: token)
    }

    // MARK: Connection

    func autoConnectIfConfigured() {
        attachLocalPushDelegate()
        if currentConfig.isComplete { connect() }
    }

    /// Loads the saved Local Push configuration(s) and attaches our delegate
    /// to every loaded instance. The framework delivers an incoming call to
    /// the delegate of a manager it LOADED from preferences: a manager the
    /// app created itself and saved is not that instance, even though it
    /// carries the same configuration. Seen 2026-09-13: after re-enabling
    /// Local Push (new object, delegate set on it, saved), the next wake to
    /// the suspended app logged "NEAppPushManager app has not set the
    /// delegate to receive the incoming call payload" and callservicesd
    /// killed the app (0xBAADCA11). So: attach at launch, again after every
    /// save, and again whenever the app leaves the foreground — the moment
    /// a delivery to this process starts to matter.
    private func attachLocalPushDelegate() {
        NEAppPushManager.loadAllFromPreferences { [weak self] managers, error in
            Task { @MainActor in
                guard let self else { return }
                if let error { self.append("Local Push: load failed: \(error.localizedDescription)"); return }
                let loaded = managers ?? []
                guard !loaded.isEmpty else { self.append("Local Push: no saved configuration"); return }
                self.pushManagers = loaded
                for m in loaded { m.delegate = self.localPushDelegate }
                self.adopt(pushManager: loaded[0])
            }
        }
    }

    /// What the Local Push section says about whether calls can arrive
    /// while the app is closed. `isActive` is only true when iOS is running
    /// the extension, which it does on a matching SSID — so "waiting for a
    /// listed Wi-Fi network" is the ordinary off-site answer, not a fault.
    static func backgroundCallState(enabled: Bool, active: Bool) -> String {
        switch (enabled, active) {
        case (false, _): return "off — calls arrive only while the app is open"
        case (true, true): return "yes — the provider is running"
        case (true, false): return "NO — waiting for a listed Wi-Fi network, or the provider needs re-enabling below"
        }
    }

    private func adopt(pushManager manager: NEAppPushManager) {
        manager.delegate = localPushDelegate
        pushManager = manager
        localPushSSIDs = manager.matchSSIDs
        // isActive: whether iOS is currently running the provider extension
        // (on a matching SSID). false here explains "callee offline, wake
        // undeliverable" on the server: nothing holds the wake connection.
        append("Local Push: delegate attached (enabled=\(manager.isEnabled), active=\(manager.isActive), ssids=\(manager.matchSSIDs))")
        backgroundCalls = Self.backgroundCallState(enabled: manager.isEnabled, active: manager.isActive)
    }

    func connect() {
        let cfg = currentConfig
        guard cfg.isComplete else { status = "incomplete settings"; return }
        do { try store.save(cfg) } catch { append("config save failed: \(error)") }
        // The same enrolment credential authenticates the SIP leg (Digest).
        engine.setCredentials(username: cfg.deviceID, password: cfg.token)

        disconnect()
        let s = GatewaySession(endpoint: cfg.gateway)
        session = s
        sessionDropped = false
        controller.attach(transport: s)
        status = "connecting to \(cfg.gateway.host):\(cfg.gateway.port)"
        eventTask = Task { [weak self] in
            for await ev in s.events {
                guard let self else { return }
                await self.handle(ev)
            }
        }
        s.connect(hello: cfg.hello(kind: .app))
    }

    func disconnect() {
        eventTask?.cancel()
        eventTask = nil
        session?.disconnect()
        session = nil
        status = "disconnected"
        sessionID = ""
    }

    /// Back in the foreground: a session that dropped while we were away
    /// reconnects now; with no session at all (never connected, or the
    /// user disconnected) nothing happens.
    private func appBecameActive() {
        session?.setActive(true)
        // Calls the extension reported while we were away.
        if let store = recentsStore {
            recents = store.foldPending()
            refreshUnseenMissed()
        }
    }

    /// Backgrounded: normally stop retrying the gateway (the extension
    /// covers wakes while iOS suspends us). NOT while a call is tracked.
    /// iOS backgrounds this app every time it puts its own call screen in
    /// front — which it does for every second incoming call — and
    /// `setActive(false)` cancels the reconnect `holdSessionWhileCallTracked`
    /// had just asked for, so the session that carries `wake_cancel` stayed
    /// down for the life of the call. On 2026-09-14 (10:43 and 10:52) the
    /// server marked the device offline mid-call within a fraction of a
    /// second of the app resigning active, and the caller's hangup had to go
    /// the long way round. CallKit keeps the process alive while a call is
    /// tracked, so retrying costs nothing there.
    private func appEnteredBackground() {
        let tracked = controller.activeCalls.count
        guard tracked == 0 else {
            append("gateway: staying connected in the background (\(tracked) call(s) tracked)")
            return
        }
        session?.setActive(false)
    }

    private func handle(_ ev: SignalEvent) async {
        switch ev {
        case .waiting(let reason):
            if reason.hasPrefix("reconnecting") {
                status = reason
                append("gateway: \(reason)")
            } else {
                status = "waiting for network (\(reason))"
                append("waiting: \(reason) — allow Local Network access if prompted")
            }
        case .connected(let w):
            status = "connected"
            sessionID = w.sessionID
            append("welcome: session \(w.sessionID), heartbeat \(w.heartbeatSeconds)s, directory v\(w.directoryVersion)")
            if sessionDropped {
                // The SIP connection died with the old session; start over
                // rather than refreshing a registration on a dead socket.
                sessionDropped = false
                engine.resetRegistration()
            }
            if let sip = w.sip {
                // Foreground path (SPEC §2): stay registered while running so
                // calls reach us directly; wakes are for the background.
                controller.setAccount(user: "\(sip.user)@\(sip.domain)",
                                      sip: SIPTarget(host: sip.host, port: sip.port, transport: sip.transport))
            }
            if w.directoryVersion != book.version { await syncDirectory() }
        case .wake, .wakeCancel:
            controller.handle(ev)
        case .directoryChanged(let v):
            append("directory changed → v\(v)")
            await syncDirectory()
        case .protocolError(let e):
            append("gateway error \(e.code.rawValue): \(e.message ?? "")")
            if e.fatal { status = "rejected: \(e.code.rawValue)" }
        case .disconnected(let reason):
            // Logged, not just shown: a session that goes down mid-call left
            // no trace in the diagnostics at all, so an incident could only
            // be read from the server's side (2026-09-14). The call count
            // says whether the drop happened while calls were up.
            append("gateway: session down (\(reason)); \(controller.activeCalls.count) call(s) tracked, app \(UIApplication.shared.applicationState == .active ? "active" : "background")")
            status = "disconnected (\(reason))"
            sessionID = ""
            sessionDropped = true
        }
    }

    /// Wake delivered by the NEAppPushProvider extension through PushKit.
    func handleExtensionWake(userInfo: [AnyHashable: Any]) {
        guard let json = userInfo["wake"] as? String,
              let wake = try? WireCoding.makeDecoder().decode(Wake.self, from: Data(json.utf8)) else {
            append("push payload without a wake: \(userInfo)")
            return
        }
        append("wake via extension for call \(wake.callID)")
        // Every PushKit delivery must be met with a CallKit report, even when
        // the app's own socket already rang this call (iOS 13+ contract).
        let name = nameIndex.name(forURI: wake.from.uri) ?? (wake.from.displayName?.isEmpty == false ? wake.from.displayName! : CallController.numberPart(of: wake.from.uri))
        let outcome = controller.handle(wake: wake)
        switch outcome {
        case .rang:
            holdSessionWhileCallTracked()
        case .duplicate(let id):
            holdSessionWhileCallTracked()
            append("wake \(wake.callID) already ringing as \(id); reaffirming with CallKit")
            if !callKit.reaffirm(callID: id) {
                // The controller still tracks the call but CallKit has no
                // entry for it (its report was refused, or it was ended on
                // the CallKit side alone). Every push must produce a report,
                // so ring it afresh under the tracked id.
                append("wake \(wake.callID): CallKit lost call \(id); reporting it again")
                callKit.reportIncoming(callID: id, displayName: name, handle: wake.from.uri) { _ in }
            }
        case .expired, .refused:
            // PushKit's contract: every push is met with a report. A wake
            // that is past its expiry, or refused (call waiting off, or two
            // calls already), is reported and ended at once.
            append("\(wake.callID): wake via extension \(outcome == .expired ? "expired" : "refused"); reporting and ending it to satisfy PushKit")
            callKit.reportIncoming(callID: wake.callID, displayName: name, handle: wake.from.uri) { [weak self] err in
                if err == nil { self?.callKit.end(callID: wake.callID, reason: .unanswered) }
            }
        }
    }

    // MARK: Directory

    func syncDirectory() async {
        let cfg = currentConfig
        let client = DirectoryClient(base: cfg.httpBase(), deviceID: cfg.deviceID, token: cfg.token,
                                     session: DirectoryClient.session(acceptAnyCertificate: cfg.gateway.acceptAnyCertificate))
        do {
            let delta = try await client.changes(since: book.version)
            book.apply(delta)
            contacts = book.sorted
            nameIndex.update(contacts)
            append("directory synced: v\(book.version), \(contacts.count) contacts")
        } catch {
            append("directory sync failed: \(error.localizedDescription)")
        }
    }

    // MARK: Local Push Connectivity (device only; SPEC §2)

    /// Saves the provider configuration for `ssids` (already parsed, see
    /// `SSIDList`): iOS runs the extension whenever the phone is joined to
    /// any one of them.
    func configureLocalPush(ssids: [String]) {
        guard !ssids.isEmpty else { localPushStatus = "no SSID given"; return }
        NEAppPushManager.loadAllFromPreferences { [weak self] managers, error in
            Task { @MainActor in
                guard let self else { return }
                if let error { self.localPushStatus = "load failed: \(error.localizedDescription)"; return }
                let manager = managers?.first ?? NEAppPushManager()
                manager.localizedDescription = "Dialler on-prem calls"
                manager.providerBundleIdentifier = DiallerIDs.pushProviderBundleID
                manager.matchSSIDs = ssids
                manager.providerConfiguration = ["gateway": "\(self.host):\(self.port)"]
                manager.isEnabled = true
                manager.saveToPreferences { err in
                    Task { @MainActor in
                        if let err {
                            let ns = err as NSError
                            let detail = ns.userInfo.map { "\($0.key)=\($0.value)" }.joined(separator: ", ")
                            self.localPushStatus = "save failed: \(ns.domain) \(ns.code) \(detail)"
                            self.append("NEAppPushManager save failed: \(ns.domain) \(ns.code) \(detail); provider=\(DiallerIDs.pushProviderBundleID)")
                        } else {
                            self.localPushStatus = "enabled for SSIDs \(SSIDList.format(ssids))"
                            self.append("NEAppPushManager saved for SSIDs \(SSIDList.format(ssids))")
                            // Not adopt(manager): deliveries go to the delegate
                            // of the instance the framework loads, so reload.
                            self.attachLocalPushDelegate()
                        }
                    }
                }
            }
        }
    }

    /// Removes any saved Local Push configuration. Stale configurations
    /// survive reinstalls and are the usual cause of NEAppPushErrorDomain 3.
    func removeLocalPush() {
        NEAppPushManager.loadAllFromPreferences { [weak self] managers, error in
            Task { @MainActor in
                guard let self else { return }
                if let error { self.localPushStatus = "load failed: \(error.localizedDescription)"; return }
                let existing = managers ?? []
                self.append("Local Push: \(existing.count) saved configuration(s)")
                guard !existing.isEmpty else { self.localPushStatus = "nothing to remove"; return }
                for m in existing {
                    m.removeFromPreferences { err in
                        Task { @MainActor in
                            self.localPushStatus = err.map { "remove failed: \($0.localizedDescription)" } ?? "removed; re-enable to save afresh"
                            if err == nil { self.localPushSSIDs = [] }
                        }
                    }
                }
            }
        }
    }

    private func append(_ line: String) {
        // notice, not info: iOS keeps info-level entries in memory only, so
        // after a kill (0xBAADCA11 for an unreported push) the lines that
        // explain it were gone. notice is persisted and shows in Console.app,
        // sysdiagnose and `log show` after the fact.
        logger.notice("\(line, privacy: .public)")
        fileLog?.write(line)
        log.append(line)
        if log.count > 200 { log.removeFirst(log.count - 200) }
    }

    /// Queues this process's and the extension's persistent logs and sends
    /// everything queued (logs, MetricKit reports) to the dev server, which
    /// files them under data/diag/<device>/. Called at launch and from the
    /// "Send diagnostics" button; failures leave the queue for next time.
    func sendDiagnostics(reason: String) async {
        guard let cfg = store.load(), cfg.isComplete else {
            diagnosticsStatus = "not configured"
            return
        }
        if let data = fileLog?.drain() { DiagnosticsClient.enqueue(kind: "app-log", name: reason, data: data) }
        if let ext = FileLog(name: "extension"), let data = ext.drain() {
            DiagnosticsClient.enqueue(kind: "extension-log", name: reason, data: data)
        }
        let client = DiagnosticsClient(base: cfg.httpBase(), deviceID: cfg.deviceID, token: cfg.token,
                                       acceptAnyCertificate: cfg.gateway.acceptAnyCertificate)
        let result = await client.flush()
        diagnosticsStatus = result.failed == 0 ? "sent \(result.sent) item(s)" : "sent \(result.sent), \(result.failed) queued (server unreachable)"
        append("diagnostics (\(reason)): \(diagnosticsStatus)")
    }
}

/// Fallback engine with no SIP stack; only logs. Kept for the simulator when
/// the baresip XCFrameworks have not been built.
final class LoggingCallEngine: CallEngine {
    var onIncomingCall: ((String, String, String?, String?) -> Void)?
    var onCallEnded: ((String, String, Int) -> Void)?
    var onOutgoingRinging: ((String, Bool) -> Void)?
    var onCallEstablished: ((String) -> Void)?
    var onTransferFailed: ((String, String) -> Void)?
    var log: (String) -> Void = { _ in }
    func register(user: String, sip: SIPTarget) {
        log("engine: would REGISTER \(user) to \(sip.host):\(sip.port)/\(sip.transport)")
    }
    func answer(engineCallID: String) {
        log("engine: would answer \(engineCallID)")
    }
    func dial(callID: String, to target: String) -> String? {
        log("engine: would dial \(target) for \(callID)")
        return "log-\(callID)"
    }
    func hangup(engineCallID: String) {
        log("engine: hangup \(engineCallID)")
    }
}

/// Receives incoming calls from the Local Push provider while this process
/// exists. Local Push Connectivity has two delivery paths: when the app is
/// not running, iOS launches it with a VoIP push (PushKit,
/// `AppDelegate.pushRegistry(_:didReceiveIncomingPushWith:...)`); when the
/// process exists — foreground or suspended — iOS calls this delegate on the
/// main queue instead. Without it the call reached nobody and callservicesd
/// killed the app (0xBAADCA11) on every wake to a suspended process, while
/// cold launches worked (device console, 2026-09-11). Both paths end in the
/// same handler, which reports to CallKit synchronously.
final class LocalPushDelegate: NSObject, NEAppPushDelegate {
    private weak var model: AppModel?

    init(model: AppModel) {
        self.model = model
    }

    func appPushManager(_ manager: NEAppPushManager, didReceiveIncomingCallWithUserInfo userInfo: [AnyHashable: Any]) {
        // Documented as delivered on the main queue; the model is main-actor.
        MainActor.assumeIsolated {
            model?.handleExtensionWake(userInfo: userInfo)
        }
    }
}

/// Receives MetricKit diagnostics (crash, hang, CPU exception, disk write)
/// and hands each payload's JSON to `onPayload`. Delivered by the system on
/// the launch following the event.
final class DiagnosticsCollector: NSObject, MXMetricManagerSubscriber {
    var onPayload: ((Data) -> Void)?

    func didReceive(_ payloads: [MXDiagnosticPayload]) {
        for p in payloads { onPayload?(p.jsonRepresentation()) }
    }

    func didReceive(_ payloads: [MXMetricPayload]) {}
}
