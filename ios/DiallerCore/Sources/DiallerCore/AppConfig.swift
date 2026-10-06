import DiallerProtocol
import Foundation

/// What the app and the extension both need to reach the gateway. Stored in
/// the App Group so the extension sees what the app configured (SPEC §4
/// "Shared framework (App Group): protocol models, keychain, config").
public struct AppConfig: Equatable, Codable, Sendable {
    public var gateway: GatewayEndpoint
    public var deviceID: String
    /// Device credential from enrolment (PROTOCOL.md §4). Stored separately
    /// in the keychain by `AppConfigStore`; kept here for in-memory use.
    public var token: String
    public var appVersion: String

    /// The version this build reports in `hello` (shown on the console's
    /// Clients page): CFBundleShortVersionString and CFBundleVersion, as
    /// Settings › Version shows them. The extension has the same numbers, so
    /// both sessions report the same thing.
    public static var bundleVersion: String {
        let info = Bundle.main.infoDictionary ?? [:]
        let short = info["CFBundleShortVersionString"] as? String ?? "0"
        let build = info["CFBundleVersion"] as? String ?? "0"
        return "\(short) (\(build))"
    }

    public init(gateway: GatewayEndpoint, deviceID: String, token: String, appVersion: String = AppConfig.bundleVersion) {
        self.gateway = gateway
        self.deviceID = deviceID
        self.token = token
        self.appVersion = appVersion
    }

    public var isComplete: Bool {
        !gateway.host.isEmpty && !deviceID.isEmpty && !token.isEmpty
    }

    public func hello(kind: ClientKind) -> Hello {
        Hello(deviceID: deviceID, token: token, client: kind, appVersion: appVersion, capabilities: ["wake", "directory"])
    }

    /// The base URL for directory calls, derived from the gateway host. TLS,
    /// on the same certificate as the gateway: the requests carry the device
    /// token, which must not cross the LAN in clear.
    public func httpBase(port: UInt16 = 8080) -> URL {
        URL(string: "https://\(gateway.host):\(port)")!
    }
}

/// Persistence for `AppConfig`: non-secret fields in App Group UserDefaults,
/// the token in the keychain under the App Group access group.
public protocol AppConfigStore {
    func load() -> AppConfig?
    func save(_ config: AppConfig) throws
    func clear()
}

/// In-memory store for tests and previews.
public final class MemoryConfigStore: AppConfigStore {
    private var config: AppConfig?
    public init(_ config: AppConfig? = nil) { self.config = config }
    public func load() -> AppConfig? { config }
    public func save(_ config: AppConfig) throws { self.config = config }
    public func clear() { config = nil }
}
