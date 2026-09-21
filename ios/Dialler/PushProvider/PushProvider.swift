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
    /// Persistent log in the App Group; the app uploads it with its own.
    private let fileLog = FileLog(name: "extension")
    /// Recents (SPEC §6 item 6): a sidecar per wake reported here, so a call
    /// the app never ran for still shows as missed. The app folds them in.
    private let recents = RecentsStore()

    private func note(_ line: String) {
        logger.notice("\(line, privacy: .public)")
        fileLog?.write(line)
    }
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
        note("start")
        queue.async { self.connect() }
    }

    override func stop(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        note("stop: \(reason.rawValue)")
        queue.async {
            self.tearDown()
            completionHandler()
        }
    }

    /// Called periodically by the system — and this, not a timer of ours, is
    /// what liveness hangs on here (SPEC §4.7).
    ///
    /// iOS need not schedule this extension, so a `DispatchSource` timer of
    /// ours may not fire; the system's own callback always does. Apple's
    /// sample checks connection health from exactly this callback
    /// (`example/`, SimplePushProvider → `checkConnectionHealth`), and
    /// `evaluateLiveness()` only reads the clock, so it is correct however
    /// irregularly it is called.
    override func handleTimerEvent() {
        queue.async {
            if self.session == nil || self.refused {
                self.note("timer: (re)connecting")
                self.connect()
            } else if self.connected {
                // Connected as far as we know — but a session can be stale
                // while looking fine (2026-09-13: disconnected 02:00→05:52
                // while iOS reported the provider active). Ask, rather than
                // assume; a dead one reports `.disconnected` and the keeper
                // takes it from there.
                self.session?.checkLiveness()
            } else {
                // Not merely a nudge: rebuild the session from scratch. On
                // 2026-09-13 the extension stayed disconnected from 02:00 to
                // 05:52 while iOS reported it active — whatever wedged the
                // keeper, a fresh transport is the sure way out and the
                // system timer is the backstop that must not fail.
                self.note("timer: not connected; rebuilding the session")
                self.connect()
            }
        }
    }

    private func connect() {
        tearDown()
        guard let cfg = AppGroupConfigStore(appGroup: DiallerIDs.appGroup).load(), cfg.isComplete else {
            note("no complete config in the App Group; the app must be set up first")
            return
        }
        let s = GatewaySession(endpoint: cfg.gateway, policy: ReconnectPolicy(delays: [0, 1, 2, 3, 5]))
        s.waitingGrace = 3
        session = s
        refused = false
        eventTask = Task { [weak self] in
            for await ev in s.events {
                guard let self else { return }
                self.queue.async { self.handle(ev, session: s) }
            }
        }
        note("connecting to \(cfg.gateway.host):\(cfg.gateway.port)")
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
            note("waiting: \(reason)")
        case .connected(let w):
            connected = true
            note("connected: session \(w.sessionID)")
        case .wake(let w):
            // notice: persisted, so the moment the extension handed a wake
            // to PushKit can be read back after the app was killed.
            note("wake for call \(w.callID)")
            // Hand the wake to the app through PushKit; the app reports CallKit.
            let json = (try? WireCoding.makeEncoder().encode(w)).map { String(decoding: $0, as: UTF8.self) } ?? "{}"
            reportIncomingCall(userInfo: ["wake": json, "call_id": w.callID])
            // The call has been surfaced to the system (PROTOCOL.md §6 step 3).
            session.send(.wakeAck(WakeAck(callID: w.callID, action: .willAnswer)))
            recents?.notePending(wake: w)
        case .wakeCancel(let c):
            note("wake cancelled: \(c.callID) \(c.reason.rawValue)")
            // The app (if running) receives the same cancel on its own socket.
            recents?.notePending(cancel: c)
        case .directoryChanged:
            break // the app syncs; the extension does not hold the address book
        case .config(let c):
            // Only the app can save the Local Push configuration; it gets
            // the same settings in its next welcome (SPEC §6 item 8b).
            note("config v\(c.version) received (ssids \(c.ssids)); the app applies it")
        case .protocolError(let e):
            note("gateway error \(e.code.rawValue) fatal=\(e.fatal)")
            // A fatal error is followed by the drop (`.disconnected`) and
            // the keeper retries — right for the transient ones (idle
            // timeout, a bad frame). Until 2026-09-13 the session machine
            // closed the transport WITHOUT the drop, so the extension logged
            // "gateway error idle_timeout fatal=true" at 04:21 and never
            // reconnected. Three codes are for good until something
            // changes: unauthorized (re-enrol in the app), an unsupported
            // protocol version (update), superseded (a newer provider
            // instance owns the session). The keeper stops its backoff on
            // those (`GatewaySession.refusals`); the system timer retries
            // them instead.
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
            note("disconnected: \(reason); the keeper will retry")
        }
    }
}
