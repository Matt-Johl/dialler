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
    /// The device's server-managed settings changed (SPEC §6 item 8b); the
    /// same body also arrives in `.connected`'s welcome.
    case config(DeviceConfig)
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
    /// Re-check that the link is alive, driven from outside the transport's
    /// own timer (SPEC §4.7). Optional; the default does nothing.
    func checkLiveness()
    var events: AsyncStream<SignalEvent> { get }
}

public extension SignalTransport {
    func checkLiveness() {}
}

/// Where to reach the gateway and how to trust it.
public struct GatewayEndpoint: Equatable, Codable, Sendable {
    public var host: String
    public var port: UInt16
    /// Development only: accept the server's self-signed certificate.
    public var acceptAnyCertificate: Bool
    /// The server certificate's SHA-256 (base64url), learned at enrolment
    /// (SPEC §4.8): when set, this certificate and no other is trusted, on
    /// the signal socket and on HTTPS alike, whatever `acceptAnyCertificate`
    /// says.
    public var certSHA256: String?

    public init(host: String, port: UInt16 = 7443, acceptAnyCertificate: Bool = false, certSHA256: String? = nil) {
        self.host = host
        self.port = port
        self.acceptAnyCertificate = acceptAnyCertificate
        self.certSHA256 = certSHA256
    }
}
