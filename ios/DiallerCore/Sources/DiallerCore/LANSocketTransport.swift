import DiallerProtocol
import Foundation
import Network

/// The production signal transport: one TLS connection to the gateway over
/// Network.framework, length-prefixed frames, hello/welcome, heartbeats.
/// Used unchanged by the foreground app and by the NEAppPushProvider
/// extension (SPEC §2). Wire semantics live in `SessionMachine`; this class
/// only moves bytes and timers.
public final class LANSocketTransport: SignalTransport {
    public let events: AsyncStream<SignalEvent>
    private let continuation: AsyncStream<SignalEvent>.Continuation

    private let endpoint: GatewayEndpoint
    private let queue = DispatchQueue(label: "dialler.lansocket")
    private var connection: NWConnection?
    private var machine = SessionMachine()
    private var decoder = FrameDecoder()
    private var heartbeat: DispatchSourceTimer?
    private var pendingHello: Hello?

    public init(endpoint: GatewayEndpoint) {
        self.endpoint = endpoint
        var c: AsyncStream<SignalEvent>.Continuation!
        events = AsyncStream { c = $0 }
        continuation = c
    }

    deinit { continuation.finish() }

    // MARK: SignalTransport

    public func connect(hello: Hello) {
        queue.async { self.open(hello: hello) }
    }

    public func send(_ message: Message) {
        queue.async { self.write(message) }
    }

    public func disconnect() {
        queue.async { self.close(reason: "client disconnect") }
    }

    // MARK: - Internals (all on `queue`)

    private func open(hello: Hello) {
        close(reason: "reconnect")
        machine = SessionMachine()
        decoder = FrameDecoder()
        pendingHello = hello

        let tls = NWProtocolTLS.Options()
        sec_protocol_options_set_min_tls_protocol_version(tls.securityProtocolOptions, .TLSv12)
        if endpoint.acceptAnyCertificate {
            sec_protocol_options_set_verify_block(tls.securityProtocolOptions, { _, _, complete in
                complete(true) // dev: self-signed server cert
            }, queue)
        }
        let params = NWParameters(tls: tls)
        let conn = NWConnection(host: .init(endpoint.host), port: .init(rawValue: endpoint.port)!, using: params)
        connection = conn

        conn.stateUpdateHandler = { [weak self] state in
            guard let self else { return }
            switch state {
            case .ready:
                if let hello = self.pendingHello {
                    self.pendingHello = nil
                    self.apply(self.machine.didOpen(hello: hello))
                }
                self.receiveLoop(conn)
            case .failed(let err):
                self.apply(self.machine.didClose(reason: "failed: \(err)"))
                self.teardown()
            case .cancelled:
                self.apply(self.machine.didClose(reason: "cancelled"))
                self.teardown()
            case .waiting(let err):
                // Path not viable yet (permission prompt, Wi-Fi coming up).
                // Network.framework retries by itself; report, don't tear down.
                self.continuation.yield(.waiting(reason: "\(err)"))
            default:
                break
            }
        }
        conn.start(queue: queue)
    }

    private func receiveLoop(_ conn: NWConnection) {
        conn.receive(minimumIncompleteLength: 1, maximumLength: 64 * 1024) { [weak self] data, _, isComplete, error in
            guard let self, self.connection === conn else { return }
            if let data, !data.isEmpty {
                self.decoder.append(data)
                do {
                    while let env = try self.decoder.nextEnvelope() {
                        self.apply(self.machine.received(env))
                    }
                } catch {
                    self.apply(self.machine.didClose(reason: "bad frame: \(error)"))
                    self.teardown()
                    return
                }
            }
            if isComplete || error != nil {
                self.apply(self.machine.didClose(reason: error.map { "read: \($0)" } ?? "server closed"))
                self.teardown()
                return
            }
            self.receiveLoop(conn)
        }
    }

    private func write(_ message: Message) {
        guard let conn = connection else { return }
        let env = Envelope(id: Self.newID(), ts: Date(), message: message)
        do {
            let frame = try Frame.encode(env)
            conn.send(content: frame, completion: .contentProcessed { [weak self] err in
                if let err, let self {
                    self.apply(self.machine.didClose(reason: "write: \(err)"))
                    self.teardown()
                }
            })
        } catch {
            apply(machine.didClose(reason: "encode: \(error)"))
            teardown()
        }
    }

    private func apply(_ actions: [SessionMachine.Action]) {
        for a in actions {
            switch a {
            case .emit(let e):
                continuation.yield(e)
            case .send(let m):
                write(m)
            case .scheduleHeartbeat(let s):
                scheduleHeartbeat(seconds: s)
            case .close:
                teardown()
            }
        }
    }

    private func scheduleHeartbeat(seconds: Int) {
        heartbeat?.cancel()
        let t = DispatchSource.makeTimerSource(queue: queue)
        t.schedule(deadline: .now() + .seconds(max(1, seconds)))
        t.setEventHandler { [weak self] in
            guard let self else { return }
            self.apply(self.machine.heartbeatDue())
        }
        t.resume()
        heartbeat = t
    }

    private func close(reason: String) {
        apply(machine.didClose(reason: reason))
        teardown()
    }

    private func teardown() {
        heartbeat?.cancel()
        heartbeat = nil
        connection?.stateUpdateHandler = nil
        connection?.cancel()
        connection = nil
    }

    private static func newID() -> String {
        "ios-" + String(UInt64(Date().timeIntervalSince1970 * 1_000_000), radix: 36) + "-" + String(UInt32.random(in: 0...UInt32.max), radix: 36)
    }
}
