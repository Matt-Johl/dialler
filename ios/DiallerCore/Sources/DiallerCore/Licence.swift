import CryptoKit
import Foundation

/// What the phone makes of the server's licence token (SPEC §4.9).
///
/// The server sends its product licence in every welcome. The phone checks
/// the vendor's signature with the key compiled in here and the expiry
/// against its own clock, and treats anything short of valid exactly as
/// the server's own `unauthorized`. The server is in the customer's hands
/// and can be patched or have its clock set back; this build cannot, which
/// is why the check lives here as well.
public enum LicenceVerdict: Equatable, Sendable {
    case valid(validUntil: Date)
    /// No token in the welcome: an older server, or a patched one.
    case missing
    /// A token that is not a licence, or whose signature is not the vendor's.
    case invalid
    /// A genuine licence whose date has passed.
    case expired

    /// The words the session reports, the same strings the server uses for
    /// its own refusals so the app has one place to turn them into copy.
    public var reason: String? {
        switch self {
        case .valid: return nil
        case .missing: return "licence missing"
        case .invalid: return "licence invalid"
        case .expired: return "licence expired"
        }
    }
}

public enum LicenceVerifier {
    /// The vendor's Ed25519 public key, the same 32 bytes as the server's
    /// `vendorkey.go`. Rotating it strands every licence issued under the
    /// old one, so it is a decision, not a routine.
    public static let vendorPublicKey = Data(base64Encoded: "i/rXva6yT8+f+Yku+odVRoQTkEw1w2rTAriuI8yWfos=")!

    static let prefix = "DP1."
    static let maxLength = 4096

    /// Verify a token: signature with `key`, then `valid_until` against `now`.
    public static func verify(_ token: String?, now: Date, key: Data = vendorPublicKey) -> LicenceVerdict {
        guard let token, !token.isEmpty else { return .missing }
        guard token.count <= maxLength, token.hasPrefix(prefix) else { return .invalid }
        let parts = token.dropFirst(prefix.count).split(separator: ".", omittingEmptySubsequences: false)
        guard parts.count == 2, !parts[0].isEmpty, !parts[1].isEmpty,
              let payload = base64URLDecode(String(parts[0])),
              let signature = base64URLDecode(String(parts[1])), signature.count == 64,
              let publicKey = try? Curve25519.Signing.PublicKey(rawRepresentation: key) else { return .invalid }
        // The signature covers the ASCII the verifier holds, prefix included.
        guard publicKey.isValidSignature(signature, for: Data((prefix + parts[0]).utf8)) else { return .invalid }
        guard let fields = try? JSONSerialization.jsonObject(with: payload) as? [String: Any],
              fields["v"] as? Int == 1,
              let until = fields["valid_until"] as? String, let validUntil = parseDate(until) else { return .invalid }
        guard validUntil > now else { return .expired }
        return .valid(validUntil: validUntil)
    }

    static func base64URLDecode(_ s: String) -> Data? {
        var b = s.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        while b.count % 4 != 0 { b.append("=") }
        return Data(base64Encoded: b)
    }

    /// RFC 3339 as the server writes it, with or without fractional seconds.
    static func parseDate(_ s: String) -> Date? {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        if let d = f.date(from: s) { return d }
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f.date(from: s)
    }
}
