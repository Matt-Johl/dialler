import DiallerCore
import DiallerProtocol
import Foundation
import NetworkExtension
import os

/// The Local Push Connectivity extension (SPEC §2): while the device is on
/// the configured SSID, iOS keeps this provider running. It holds the same
/// wire-protocol TLS connection the foreground app uses (kind "extension"),
/// and on a wake reports the call to the system, which launches the app via
/// PushKit and CallKit. Signalling only — no media here.
final class PushProvider: NEAppPushProvider {
    private let logger = Logger(subsystem: DiallerIDs.bundlePrefix, category: "push-provider")
    private var transport: LANSocketTransport?
    private var eventTask: Task<Void, Never>?
    private var connected = false

    override func start() {
        logger.info("start")
        connect()
    }

    override func stop(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        logger.info("stop: \(reason.rawValue)")
        tearDown()
        completionHandler()
    }

    /// Called periodically by the system; used to reconnect after a drop.
    override func handleTimerEvent() {
        if !connected {
            logger.info("timer: reconnecting")
            connect()
        }
    }

    private func connect() {
        tearDown()
        guard let cfg = AppGroupConfigStore(appGroup: DiallerIDs.appGroup).load(), cfg.isComplete else {
            logger.error("no complete config in the App Group; the app must be set up first")
            return
        }
        let t = LANSocketTransport(endpoint: cfg.gateway)
        transport = t
        eventTask = Task { [weak self] in
            for await ev in t.events {
                guard let self else { return }
                self.handle(ev, transport: t)
            }
        }
        t.connect(hello: cfg.hello(kind: .extensionKind))
    }

    private func tearDown() {
        eventTask?.cancel()
        eventTask = nil
        transport?.disconnect()
        transport = nil
        connected = false
    }

    private func handle(_ ev: SignalEvent, transport: LANSocketTransport) {
        switch ev {
        case .waiting(let reason):
            logger.info("waiting: \(reason, privacy: .public)")
        case .connected(let w):
            connected = true
            logger.info("connected: session \(w.sessionID, privacy: .public)")
        case .wake(let w):
            logger.info("wake for call \(w.callID, privacy: .public)")
            // Hand the wake to the app through PushKit; the app reports CallKit.
            let json = (try? WireCoding.makeEncoder().encode(w)).map { String(decoding: $0, as: UTF8.self) } ?? "{}"
            reportIncomingCall(userInfo: ["wake": json, "call_id": w.callID])
            // The call has been surfaced to the system (PROTOCOL.md §6 step 3).
            transport.send(.wakeAck(WakeAck(callID: w.callID, action: .willAnswer)))
        case .wakeCancel(let c):
            logger.info("wake cancelled: \(c.callID, privacy: .public) \(c.reason.rawValue, privacy: .public)")
            // The app (if running) receives the same cancel on its own socket.
        case .directoryChanged:
            break // the app syncs; the extension does not hold the address book
        case .protocolError(let e):
            logger.error("gateway error \(e.code.rawValue, privacy: .public) fatal=\(e.fatal)")
            if e.fatal { connected = false }
        case .disconnected(let reason):
            logger.warning("disconnected: \(reason, privacy: .public)")
            connected = false
        }
    }
}
