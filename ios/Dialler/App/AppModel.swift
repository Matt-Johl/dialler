import AVFoundation
import Combine
import DiallerCore
import DiallerProtocol
import Foundation
import NetworkExtension
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
    @Published private(set) var log: [String] = []
    @Published private(set) var localPushStatus = "not configured"

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
        var status: String {
            if connectedAt == nil { return outgoing ? "Calling…" : "Connecting…" }
            return held ? "On hold" : "Connected"
        }
    }
    @Published private(set) var activeCall: ActiveCall?

    private let store: AppConfigStore = AppGroupConfigStore(appGroup: DiallerIDs.appGroup)
    private let callKit = CallKitBridge()
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
    private lazy var localPushDelegate = LocalPushDelegate(model: self)
    private var book = AddressBook()
    /// Thread-safe caller-name lookup for incoming calls (the controller
    /// resolves names off the main actor). Kept in step with `contacts`.
    private nonisolated let nameIndex = DirectoryNameIndex()
    private let logger = Logger(subsystem: DiallerIDs.bundlePrefix, category: "app")

    init() {
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
        callKit.onAnswer = { [weak self] id in
            guard let self else { return }
            self.controller.userAnswered(callID: id)
            let title = self.controller.activeCalls.first { $0.wake.callID == id }.map { self.title(for: $0) } ?? "Call"
            self.activeCall = ActiveCall(id: id, title: title, outgoing: false, connectedAt: Date())
        }
        callKit.onEnd = { [weak self] id in
            guard let self else { return }
            self.controller.userEnded(callID: id)
            if self.activeCall?.id == id { self.activeCall = nil }
        }
        callKit.onStart = { [weak self] id in
            guard let self else { return }
            self.controller.userStarted(callID: id)
            let title = self.controller.activeCalls.first { $0.wake.callID == id }.map { self.title(for: $0) } ?? "Call"
            self.activeCall = ActiveCall(id: id, title: title, outgoing: true, connectedAt: nil)
        }
        callKit.onEnded = { [weak self] id in self?.callEnded(id) }
        callKit.onStartFailed = { [weak self] id in
            self?.controller.startFailed(callID: id)
            if self?.activeCall?.id == id { self?.activeCall = nil }
        }
        callKit.onConnected = { [weak self] id in
            guard let self, self.activeCall?.id == id else { return }
            self.activeCall?.connectedAt = Date()
        }
        callKit.onMute = { [weak self] id, muted in
            guard let self else { return }
            self.controller.setMuted(muted)
            if self.activeCall?.id == id { self.activeCall?.muted = muted }
        }
        controller.onTransferFailed = { [weak self] reason in Task { @MainActor in self?.append("transfer refused: \(reason)") } }
        // Show the directory's friendly name for a known incoming caller
        // (e.g. "SIP phone (101)" instead of sip:101@…). The controller runs
        // this off the main actor, so read a snapshot of the contacts.
        controller.resolveDisplayName = { [weak self] uri, provided in
            self?.nameIndex.name(forURI: uri) ?? provided
        }
        callKit.onHold = { [weak self] id, held in
            guard let self else { return }
            self.controller.setHeld(callID: id, held)
            if self.activeCall?.id == id { self.activeCall?.held = held }
        }
        callKit.onAudioActivated = { [weak self] in self?.engine.audioSessionActivated() }
        callKit.onAudioDeactivated = { [weak self] in self?.engine.audioSessionDeactivated() }
        callKit.onLog = { [weak self] m in Task { @MainActor in self?.append(m) } }
        // iOS suspends the app (and kills its sockets) in the background;
        // reconnect the moment we are back, and do not retry while away.
        let nc = NotificationCenter.default
        nc.addObserver(forName: UIApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.appBecameActive() }
        }
        nc.addObserver(forName: UIApplication.didEnterBackgroundNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.session?.setActive(false) }
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
        guard var call = activeCall else { return }
        call.speaker.toggle()
        do {
            try AVAudioSession.sharedInstance().overrideOutputAudioPort(call.speaker ? .speaker : .none)
            activeCall = call
        } catch {
            append("speaker toggle failed: \(error.localizedDescription)")
        }
    }

    /// CallKit ended a call that the in-call screen is showing (remote
    /// hangup, failure) — the bridge reports it through the controller's
    /// `end`, which calls this.
    func callEnded(_ id: String) {
        if activeCall?.id == id { activeCall = nil }
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

    /// Loads the saved Local Push configuration and attaches our delegate to
    /// it. Must happen at every launch: the delegate lives on the loaded
    /// object, not in the saved preferences.
    private func attachLocalPushDelegate() {
        NEAppPushManager.loadAllFromPreferences { [weak self] managers, error in
            Task { @MainActor in
                guard let self else { return }
                if let error { self.append("Local Push: load failed: \(error.localizedDescription)"); return }
                guard let manager = managers?.first else { return }
                self.adopt(pushManager: manager)
            }
        }
    }

    private func adopt(pushManager manager: NEAppPushManager) {
        manager.delegate = localPushDelegate
        pushManager = manager
        append("Local Push: delegate attached (enabled=\(manager.isEnabled), ssids=\(manager.matchSSIDs))")
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
        switch controller.handle(wake: wake) {
        case .rang:
            break
        case .duplicate(let id):
            append("wake \(wake.callID) already ringing as \(id); reaffirming with CallKit")
            if !callKit.reaffirm(callID: id) {
                // The controller still tracks the call but CallKit has no
                // entry for it (its report was refused, or it was ended on
                // the CallKit side alone). Every push must produce a report,
                // so ring it afresh under the tracked id.
                append("wake \(wake.callID): CallKit lost call \(id); reporting it again")
                callKit.reportIncoming(callID: id, displayName: name, handle: wake.from.uri) { _ in }
            }
        case .expired:
            append("expired wake via extension; reporting and ending \(wake.callID) to satisfy PushKit")
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

    func configureLocalPush(ssid: String) {
        NEAppPushManager.loadAllFromPreferences { [weak self] managers, error in
            Task { @MainActor in
                guard let self else { return }
                if let error { self.localPushStatus = "load failed: \(error.localizedDescription)"; return }
                let manager = managers?.first ?? NEAppPushManager()
                manager.localizedDescription = "Dialler on-prem calls"
                manager.providerBundleIdentifier = DiallerIDs.pushProviderBundleID
                manager.matchSSIDs = [ssid]
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
                            self.localPushStatus = "enabled for SSID \(ssid)"
                            self.append("NEAppPushManager saved for SSID \(ssid)")
                            self.adopt(pushManager: manager)
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
        log.append(line)
        if log.count > 200 { log.removeFirst(log.count - 200) }
    }
}

/// Fallback engine with no SIP stack; only logs. Kept for the simulator when
/// the baresip XCFrameworks have not been built.
final class LoggingCallEngine: CallEngine {
    var onIncomingCall: ((String, String?) -> Void)?
    var onCallEnded: ((String) -> Void)?
    var onOutgoingRinging: (() -> Void)?
    var onCallEstablished: (() -> Void)?
    var onTransferFailed: ((String) -> Void)?
    var log: (String) -> Void = { _ in }
    func register(user: String, sip: SIPTarget) {
        log("engine: would REGISTER \(user) to \(sip.host):\(sip.port)/\(sip.transport)")
    }
    func prepareForIncomingCall(callID: String, user: String, sip: SIPTarget) {
        log("engine: would answer call \(callID) as \(user) via \(sip.host):\(sip.port)/\(sip.transport)")
    }
    func dial(callID: String, to target: String) {
        log("engine: would dial \(target) for \(callID)")
    }
    func hangup(callID: String) {
        log("engine: hangup \(callID)")
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
