import DiallerProtocol
import Foundation

/// What a signal transport delivers to the app. Both the foreground LAN
/// socket and the extension socket produce exactly these; APNS later maps
/// its payload onto the same cases (SPEC §5 `SignalTransport` seam).
public enum SignalEvent: Equatable, Sendable {
    /// The connection is not established yet and the system is waiting for
    /// the path to become viable — e.g. the Local Network permission prompt
    /// is showing, or Wi-Fi is reconnecting. Not a failure; keep waiting.
    case waiting(reason: String)
    case connected(Welcome)
    case wake(Wake)
    case wakeCancel(WakeCancel)
    case directoryChanged(Int64)
    case protocolError(ProtocolError)
    case disconnected(reason: String)
}

/// A live connection to the gateway. Implementations: `LANSocketTransport`
/// (Network.framework TLS, production) and `FakeTransport` (tests).
public protocol SignalTransport: AnyObject {
    /// Opens the connection and performs the hello/welcome handshake.
    /// Events, starting with `.connected`, arrive on `events`.
    func connect(hello: Hello)
    /// Sends one message on the live connection. Silently dropped if not live.
    func send(_ message: Message)
    func disconnect()
    var events: AsyncStream<SignalEvent> { get }
}

/// Where to reach the gateway and how to trust it.
public struct GatewayEndpoint: Equatable, Codable, Sendable {
    public var host: String
    public var port: UInt16
    /// Development only: accept the server's self-signed certificate.
    public var acceptAnyCertificate: Bool

    public init(host: String, port: UInt16 = 7443, acceptAnyCertificate: Bool = false) {
        self.host = host
        self.port = port
        self.acceptAnyCertificate = acceptAnyCertificate
    }
}
