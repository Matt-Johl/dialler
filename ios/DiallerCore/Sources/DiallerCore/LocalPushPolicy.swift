import DiallerProtocol
import Foundation

/// What to do with the Local Push configuration when the server's settings
/// for this device arrive (SPEC §6 item 8b). Pure, so it is tested; the
/// app performs the action on `NEAppPushManager`.
///
/// Two rules matter. A phone the administrator has never configured is
/// left alone (no `config` in the welcome): its own list, if any, keeps
/// working. And a configuration is saved only when the list actually
/// differs from what the provider holds — re-saving an identical one can
/// restart the provider and drop the connection it is holding.
public enum LocalPushPolicy {
    public enum Action: Equatable, Sendable {
        /// Nothing to do.
        case leave
        /// Save the provider configuration with these SSIDs, enabled.
        case save([String])
        /// Remove the saved configuration: no background wakes.
        case remove
    }

    /// `received` is the server's config (nil when it has none for this
    /// device); `current` the provider's saved SSIDs (nil when there is no
    /// saved configuration) and whether it is enabled.
    public static func plan(received: DeviceConfig?, current: [String]?, enabled: Bool) -> Action {
        guard let received else { return .leave }
        let wanted = SSIDList.parse(received.ssids.joined(separator: ","))
        if wanted.isEmpty {
            return current == nil ? .leave : .remove
        }
        if let current, enabled, Set(current) == Set(wanted) {
            return .leave
        }
        return .save(wanted)
    }
}
