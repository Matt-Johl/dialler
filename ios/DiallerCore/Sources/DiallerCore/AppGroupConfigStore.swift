import Foundation
import Security

/// `AppConfigStore` backed by the App Group: UserDefaults for host/port/
/// device id, keychain (shared access group) for the token. Works in the
/// simulator and on device for both the app and the extension.
public final class AppGroupConfigStore: AppConfigStore {
    private let group: String
    private let defaults: UserDefaults
    private let tokenAccount = "device-token"

    public init(appGroup: String) {
        group = appGroup
        defaults = UserDefaults(suiteName: appGroup) ?? .standard
    }

    private enum Key {
        static let host = "gateway.host"
        static let port = "gateway.port"
        static let acceptAny = "gateway.acceptAnyCertificate"
        static let deviceID = "device.id"
    }

    public func load() -> AppConfig? {
        guard let host = defaults.string(forKey: Key.host), !host.isEmpty,
              let deviceID = defaults.string(forKey: Key.deviceID), !deviceID.isEmpty else { return nil }
        let port = UInt16(clamping: defaults.integer(forKey: Key.port))
        let token = readToken() ?? ""
        return AppConfig(
            gateway: GatewayEndpoint(host: host, port: port == 0 ? 7443 : port, acceptAnyCertificate: defaults.bool(forKey: Key.acceptAny)),
            deviceID: deviceID,
            token: token
        )
    }

    public func save(_ config: AppConfig) throws {
        defaults.set(config.gateway.host, forKey: Key.host)
        defaults.set(Int(config.gateway.port), forKey: Key.port)
        defaults.set(config.gateway.acceptAnyCertificate, forKey: Key.acceptAny)
        defaults.set(config.deviceID, forKey: Key.deviceID)
        try writeToken(config.token)
    }

    public func clear() {
        for k in [Key.host, Key.port, Key.acceptAny, Key.deviceID] { defaults.removeObject(forKey: k) }
        SecItemDelete(baseQuery() as CFDictionary)
    }

    // MARK: keychain

    private func baseQuery() -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: "dialler.gateway",
            kSecAttrAccount as String: tokenAccount,
            kSecAttrAccessGroup as String: group,
        ]
    }

    private func readToken() -> String? {
        var q = baseQuery()
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: AnyObject?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        guard status == errSecSuccess, let data = out as? Data else { return nil }
        return String(decoding: data, as: UTF8.self)
    }

    private func writeToken(_ token: String) throws {
        let data = Data(token.utf8)
        var add = baseQuery()
        add[kSecValueData as String] = data
        add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlock // extension may run before unlock
        var status = SecItemAdd(add as CFDictionary, nil)
        if status == errSecDuplicateItem {
            status = SecItemUpdate(baseQuery() as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        }
        guard status == errSecSuccess else {
            throw NSError(domain: NSOSStatusErrorDomain, code: Int(status), userInfo: [NSLocalizedDescriptionKey: "keychain write failed (\(status))"])
        }
    }
}
