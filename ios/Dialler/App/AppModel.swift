import Combine
import DiallerCore
import DiallerProtocol
import Foundation
import NetworkExtension
import os
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

    private let store: AppConfigStore = AppGroupConfigStore(appGroup: DiallerIDs.appGroup)
    private let callKit = CallKitBridge()
    private let engine: CallEngine
    @Published private(set) var engineState = "no engine"
    private lazy var controller = CallController(ui: callKit, engine: engine, log: { [weak self] m in
        Task { @MainActor in self?.append(m) }
    })
    private var transport: LANSocketTransport?
    private var eventTask: Task<Void, Never>?
    private var book = AddressBook()
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
        callKit.onAnswer = { [weak self] id in self?.controller.userAnswered(callID: id) }
        callKit.onEnd = { [weak self] id in self?.controller.userEnded(callID: id) }
        callKit.onAudioActivated = { [weak self] in self?.engine.audioSessionActivated() }
        callKit.onAudioDeactivated = { [weak self] in self?.engine.audioSessionDeactivated() }
        callKit.onLog = { [weak self] m in Task { @MainActor in self?.append(m) } }
        #if canImport(DiallerEngine)
        baresip.log = { [weak self] m in Task { @MainActor in self?.append(m) } }
        baresip.onStateChange = { [weak self] s in Task { @MainActor in self?.engineState = "baresip \(s)" } }
        #else
        logging.log = { [weak self] m in Task { @MainActor in self?.append(m) } }
        #endif
    }

    var currentConfig: AppConfig {
        AppConfig(gateway: GatewayEndpoint(host: host, port: UInt16(port) ?? 7443, acceptAnyCertificate: acceptAnyCertificate),
                  deviceID: deviceID, token: token)
    }

    // MARK: Connection

    func autoConnectIfConfigured() {
        if currentConfig.isComplete { connect() }
    }

    func connect() {
        let cfg = currentConfig
        guard cfg.isComplete else { status = "incomplete settings"; return }
        do { try store.save(cfg) } catch { append("config save failed: \(error)") }

        disconnect()
        let t = LANSocketTransport(endpoint: cfg.gateway)
        transport = t
        controller.attach(transport: t)
        status = "connecting to \(cfg.gateway.host):\(cfg.gateway.port)"
        eventTask = Task { [weak self] in
            for await ev in t.events {
                guard let self else { return }
                await self.handle(ev)
            }
        }
        t.connect(hello: cfg.hello(kind: .app))
    }

    func disconnect() {
        eventTask?.cancel()
        eventTask = nil
        transport?.disconnect()
        transport = nil
        status = "disconnected"
        sessionID = ""
    }

    private func handle(_ ev: SignalEvent) async {
        switch ev {
        case .waiting(let reason):
            status = "waiting for network (\(reason))"
            append("waiting: \(reason) — allow Local Network access if prompted")
        case .connected(let w):
            status = "connected"
            sessionID = w.sessionID
            append("welcome: session \(w.sessionID), heartbeat \(w.heartbeatSeconds)s, directory v\(w.directoryVersion)")
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
        switch controller.handle(wake: wake) {
        case .rang:
            break
        case .duplicate(let id):
            append("wake \(wake.callID) already ringing as \(id); reaffirming with CallKit")
            callKit.reaffirm(callID: id)
        case .expired:
            append("expired wake via extension; reporting and ending \(wake.callID) to satisfy PushKit")
            callKit.reportIncoming(callID: wake.callID, displayName: wake.from.displayName ?? wake.from.uri, handle: wake.from.uri) { [weak self] err in
                if err == nil { self?.callKit.end(callID: wake.callID, reason: .unanswered) }
            }
        }
    }

    // MARK: Directory

    func syncDirectory() async {
        let cfg = currentConfig
        let client = DirectoryClient(base: cfg.httpBase(), deviceID: cfg.deviceID, token: cfg.token)
        do {
            let delta = try await client.changes(since: book.version)
            book.apply(delta)
            contacts = book.sorted
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
        logger.info("\(line, privacy: .public)")
        log.append(line)
        if log.count > 200 { log.removeFirst(log.count - 200) }
    }
}

/// Fallback engine with no SIP stack; only logs. Kept for the simulator when
/// the baresip XCFrameworks have not been built.
final class LoggingCallEngine: CallEngine {
    var onIncomingCall: ((String) -> Void)?
    var onCallEnded: ((String) -> Void)?
    var log: (String) -> Void = { _ in }
    func register(user: String, sip: SIPTarget) {
        log("engine: would REGISTER \(user) to \(sip.host):\(sip.port)/\(sip.transport)")
    }
    func prepareForIncomingCall(callID: String, user: String, sip: SIPTarget) {
        log("engine: would answer call \(callID) as \(user) via \(sip.host):\(sip.port)/\(sip.transport)")
    }
    func hangup(callID: String) {
        log("engine: hangup \(callID)")
    }
}
