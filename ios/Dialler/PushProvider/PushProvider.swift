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
///
/// The connection is kept up by `GatewaySession`, the same keeper the app
/// uses, permanently "active": every drop is retried with a short backoff
/// (1, 2, 3, then 5 s for ever), and an attempt that lands while the server
/// is still coming up — Network.framework parks it in "waiting" and only
/// retries on a path change — is abandoned after 3 s and retried. Before
/// this the provider opened one raw transport and only retried when iOS
/// called its periodic timer, so a server restart left the extension down
/// until Wi-Fi was toggled (user-reported 2026-09-11). The timer is now only
/// a backstop that kicks the keeper.
final class PushProvider: NEAppPushProvider, @unchecked Sendable {
    // @unchecked Sendable: all mutable state is confined to `queue`; the
    // system's callbacks and the session's event pump hop onto it before
    // touching anything.
    private let logger = Logger(subsystem: DiallerIDs.bundlePrefix, category: "push-provider")
    /// Everything below runs on this queue: the NE callbacks arrive on the
    /// system's queue and the session's events on a task.
    private let queue = DispatchQueue(label: "dialler.push-provider")
    private var session: GatewaySession?
    private var eventTask: Task<Void, Never>?
    private var connected = false
    /// The server refused us for good (bad enrolment): stop hammering it and
    /// let the periodic timer retry, in case the app has been re-enrolled.
    private var refused = false

    override func start() {
        logger.notice("start")
        queue.async { self.connect() }
    }

    override func stop(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        logger.notice("stop: \(reason.rawValue)")
        queue.async {
            self.tearDown()
            completionHandler()
        }
    }

    /// Called periodically by the system. A backstop only: the keeper
    /// reconnects on its own; this catches a provider that never had a
    /// complete config, a session refused as unenrolled, or anything the
    /// keeper has somehow given up on.
    override func handleTimerEvent() {
        queue.async {
            if self.session == nil || self.refused {
                self.logger.notice("timer: (re)connecting")
                self.connect()
            } else if !self.connected {
                self.logger.notice("timer: not connected; kicking the keeper")
                self.session?.setActive(true) // reconnect now, backoff reset
            }
        }
    }

    private func connect() {
        tearDown()
        guard let cfg = AppGroupConfigStore(appGroup: DiallerIDs.appGroup).load(), cfg.isComplete else {
            logger.error("no complete config in the App Group; the app must be set up first")
            return
        }
        let s = GatewaySession(endpoint: cfg.gateway, policy: ReconnectPolicy(delays: [1, 2, 3, 5]))
        s.waitingGrace = 3
        session = s
        refused = false
        eventTask = Task { [weak self] in
            for await ev in s.events {
                guard let self else { return }
                self.queue.async { self.handle(ev, session: s) }
            }
        }
        logger.notice("connecting to \(cfg.gateway.host, privacy: .public):\(cfg.gateway.port)")
        s.connect(hello: cfg.hello(kind: .extensionKind))
    }

    private func tearDown() {
        eventTask?.cancel()
        eventTask = nil
        session?.disconnect()
        session = nil
        connected = false
    }

    private func handle(_ ev: SignalEvent, session: GatewaySession) {
        switch ev {
        case .waiting(let reason):
            // Includes the keeper's own "reconnecting in Ns" notices.
            logger.notice("waiting: \(reason, privacy: .public)")
        case .connected(let w):
            connected = true
            logger.notice("connected: session \(w.sessionID, privacy: .public)")
        case .wake(let w):
            // notice: persisted, so the moment the extension handed a wake
            // to PushKit can be read back after the app was killed.
            logger.notice("wake for call \(w.callID, privacy: .public)")
            // Hand the wake to the app through PushKit; the app reports CallKit.
            let json = (try? WireCoding.makeEncoder().encode(w)).map { String(decoding: $0, as: UTF8.self) } ?? "{}"
            reportIncomingCall(userInfo: ["wake": json, "call_id": w.callID])
            // The call has been surfaced to the system (PROTOCOL.md §6 step 3).
            session.send(.wakeAck(WakeAck(callID: w.callID, action: .willAnswer)))
        case .wakeCancel(let c):
            logger.notice("wake cancelled: \(c.callID, privacy: .public) \(c.reason.rawValue, privacy: .public)")
            // The app (if running) receives the same cancel on its own socket.
        case .directoryChanged:
            break // the app syncs; the extension does not hold the address book
        case .protocolError(let e):
            logger.error("gateway error \(e.code.rawValue, privacy: .public) fatal=\(e.fatal)")
            // The server closes the connection after any fatal error and the
            // keeper retries — right for the transient ones (idle timeout, a
            // bad frame). Three are for good until something changes:
            // unauthorized (re-enrol in the app), an unsupported protocol
            // version (update), superseded (a newer provider instance owns
            // the session). Retrying those every few seconds would only fill
            // the server log and, for superseded, fight the newer instance;
            // the system timer retries them instead.
            switch e.code {
            case .unauthorized, .unsupportedVersion, .superseded:
                refused = true
                connected = false
                session.disconnect()
            default:
                break
            }
        case .disconnected(let reason):
            connected = false
            logger.warning("disconnected: \(reason, privacy: .public); the keeper will retry")
        }
    }
}
