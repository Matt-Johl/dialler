import Foundation

/// The Wi-Fi networks Local Push Connectivity runs on, as the user types
/// them: one text field, comma-separated. `NEAppPushManager.matchSSIDs` is
/// a list and iOS runs the provider whenever the phone is joined to any
/// network in it (SPEC §2), so the only work is honest parsing. SSIDs are
/// case-sensitive and may contain spaces, so nothing is lower-cased and
/// only the ends of each entry are trimmed.
public enum SSIDList {
    /// Splits on commas, trims each entry, drops empties and repeats
    /// (first occurrence wins, order kept).
    public static func parse(_ text: String) -> [String] {
        var seen = Set<String>()
        var out: [String] = []
        for piece in text.split(separator: ",", omittingEmptySubsequences: true) {
            let ssid = piece.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !ssid.isEmpty, seen.insert(ssid).inserted else { continue }
            out.append(ssid)
        }
        return out
    }

    /// The text-field form of a list, the inverse of `parse` for any list
    /// `parse` can produce.
    public static func format(_ ssids: [String]) -> String {
        ssids.joined(separator: ", ")
    }
}
