import Foundation

/// What an outgoing call is doing, for the app's own in-call screen.
///
/// CallKit shows nothing useful here — its UI says "calling" until the call
/// ends — so the screen said "Calling…" while the earpiece was already
/// playing congestion at someone (2026-09-15). The controller knows the
/// truth from SIP; this is that knowledge in the smallest form the screen
/// needs.
///
/// The words live here rather than in the view because the app target has
/// no tests: a rule kept there is a rule nobody checks (the same reason
/// `TonePolicy` is in this module).
public enum CallProgress: Equatable, Sendable {
    /// Dialled, and nothing has come back yet. Our server sends 180 only
    /// when something is genuinely alerting (SPEC §4.4 rule 7b), so this
    /// really does mean "no idea yet", not "ringing".
    case calling
    /// The far end is alerting, or sending early media.
    case ringing
    /// Busy Here / Busy Everywhere.
    case busy
    /// The person refused the call.
    case declined
    /// Nobody answered in time.
    case noAnswer
    /// Registered nowhere, switched off, or the PBX could not reach it.
    case unavailable
    /// There is no such number.
    case unknownNumber
    /// Refused for a reason none of the above describes.
    case failed

    /// What the in-call screen puts under the name.
    public var label: String {
        switch self {
        case .calling: return "Calling…"
        case .ringing: return "Ringing…"
        case .busy: return "Busy"
        case .declined: return "Declined"
        case .noAnswer: return "No answer"
        case .unavailable: return "Unavailable"
        case .unknownNumber: return "Unknown number"
        case .failed: return "Call failed"
        }
    }

    /// Whether the call is over — the screen keeps this on display for as
    /// long as its tone plays, then the call disappears.
    public var isFailure: Bool {
        switch self {
        case .calling, .ringing: return false
        default: return true
        }
    }

    /// The same, from the text a transfer failure carries.
    ///
    /// Transfers are the one place the status does not arrive as a number.
    /// baresip reports the far end's NOTIFY sipfrag verbatim — `"%u %r"`,
    /// so `"486 Busy Here"` — and leaves `call_scode` alone, because that
    /// belongs to the referring call and not to the transfer target. A
    /// local failure has no status at all (`"%m"` of an errno), which is
    /// why this returns nil rather than guessing.
    public static func forTransferFailure(reason: String) -> CallProgress? {
        let digits = reason.prefix { $0.isNumber }
        guard digits.count == 3, let status = Int(digits) else { return nil }
        return forFailure(status: status)
    }

    /// How an outgoing call that ended with SIP status `status` should be
    /// described, or nil when the ending deserves no explanation: a normal
    /// clear (0) or the 487 answering the user's own cancel, exactly as
    /// `CallTones.failure(status:)` stays silent for those.
    public static func forFailure(status: Int) -> CallProgress? {
        switch status {
        case 486, 600: return .busy
        case 603: return .declined
        case 408: return .noAnswer
        case 404, 410: return .unknownNumber
        case 480, 503: return .unavailable
        case 487: return nil // our own CANCEL
        case 400...: return .failed
        default: return nil
        }
    }
}
