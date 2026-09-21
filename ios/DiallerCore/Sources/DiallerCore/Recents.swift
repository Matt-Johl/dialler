import DiallerProtocol
import Foundation

/// One finished call, as the Recents list shows it (SPEC §6 item 6).
///
/// Written by the controller the moment a call leaves its table — every
/// path out of it, answered or not — and by the extension for a wake the
/// app never ran for. Device-local: the server keeps no call records.
public struct CallRecord: Codable, Equatable, Identifiable, Sendable {
    public enum Direction: String, Codable, Sendable { case incoming, outgoing }

    /// How the call ended. The classification is `outcome(direction:answered:ending:)`.
    public enum Outcome: String, Codable, Sendable {
        /// Answered, on either side; `duration` says for how long.
        case completed
        /// Incoming, and nobody here answered before the caller gave up or
        /// the ring timed out — or the app could not ring it at all.
        case missed
        /// Incoming and refused here; outgoing and the far end refused (603).
        case declined
        /// Outgoing and hung up here before the far end answered.
        case cancelled
        /// Incoming, and another device of the same user took it.
        case answeredElsewhere
        // Outgoing failures, the same set the in-call screen explains.
        case busy, noAnswer, unknownNumber, unavailable, failed
    }

    /// The id the controller tracked the call under (the wake's id, or a
    /// synthetic one for a call that rang from its INVITE first).
    public var id: String
    /// The server's id when it differs from `id` (INVITE-first calls): a
    /// record from either process is the same call if either id matches.
    public var wakeCallID: String?
    public var direction: Direction
    /// The other party: the caller, or whoever was dialled. `displayName`
    /// is the name as known when the call ended; the list looks the URI up
    /// in the directory again at render time and falls back to this.
    public var counterpart: Party
    public var startedAt: Date
    /// When the conversation began; nil for a call that never connected.
    public var connectedAt: Date?
    public var endedAt: Date
    public var outcome: Outcome

    public init(id: String, wakeCallID: String? = nil, direction: Direction, counterpart: Party,
                startedAt: Date, connectedAt: Date? = nil, endedAt: Date, outcome: Outcome) {
        self.id = id
        self.wakeCallID = wakeCallID
        self.direction = direction
        self.counterpart = counterpart
        self.startedAt = startedAt
        self.connectedAt = connectedAt
        self.endedAt = endedAt
        self.outcome = outcome
    }

    /// Seconds of conversation; nil when there was none.
    public var duration: TimeInterval? {
        connectedAt.map { max(0, endedAt.timeIntervalSince($0)) }
    }

    public var isMissed: Bool { outcome == .missed }

    /// Whether `callID` names this call, by either of its ids.
    public func matches(callID: String) -> Bool {
        id == callID || wakeCallID == callID
    }

    /// The bare number of the other party, for the row's second line.
    public var number: String { CallController.numberPart(of: counterpart.uri) }

    /// What the row says on the right: the duration of a conversation, or
    /// why there was none.
    public var summary: String {
        if let duration { return Self.durationText(duration) }
        return outcome.label
    }

    /// "0:42", "12:05", "1:03:10".
    public static func durationText(_ seconds: TimeInterval) -> String {
        let s = max(0, Int(seconds.rounded()))
        return s >= 3600
            ? String(format: "%d:%02d:%02d", s / 3600, s / 60 % 60, s % 60)
            : String(format: "%d:%02d", s / 60, s % 60)
    }

    // MARK: Classification

    /// What ended a call, from the controller's point of view.
    public enum Ending: Equatable, Sendable {
        /// The SIP leg ended it: a BYE or CANCEL (`status` 0), or the
        /// response that refused our outgoing call.
        case sip(status: Int)
        /// The user acted in the system UI: hung up, declined, or cancelled
        /// a call of their own.
        case user
        /// The server withdrew the wake (`wake_cancel`).
        case cancelled(CancelReason)
        /// The app refused the wake (call waiting off, no room) or it had
        /// expired before it arrived: the caller never rang here.
        case refused
        /// The call could not be started or shown at all.
        case failed
    }

    /// The classification table (SPEC §6 item 6). Answered calls are
    /// complete whatever ended them; the rest depends on direction.
    public static func outcome(direction: Direction, answered: Bool, ending: Ending) -> Outcome {
        if answered { return .completed }
        switch direction {
        case .incoming:
            switch ending {
            case .user: return .declined
            case .cancelled(.answeredElsewhere): return .answeredElsewhere
            case .sip, .cancelled, .refused, .failed: return .missed
            }
        case .outgoing:
            switch ending {
            case .user: return .cancelled
            case .cancelled: return .cancelled
            case .refused, .failed: return .failed
            case .sip(let status):
                switch CallProgress.forFailure(status: status) {
                case .busy?: return .busy
                case .declined?: return .declined
                case .noAnswer?: return .noAnswer
                case .unknownNumber?: return .unknownNumber
                case .unavailable?: return .unavailable
                case .failed?: return .failed
                // 487 answers our own CANCEL; anything else with no status
                // ended before it was ever answered.
                case nil: return status == 487 ? .cancelled : .failed
                default: return .failed
                }
            }
        }
    }
}

public extension CallRecord.Outcome {
    var label: String {
        switch self {
        case .completed: return "Completed"
        case .missed: return "Missed"
        case .declined: return "Declined"
        case .cancelled: return "Cancelled"
        case .answeredElsewhere: return "Answered elsewhere"
        case .busy: return "Busy"
        case .noAnswer: return "No answer"
        case .unknownNumber: return "Unknown number"
        case .unavailable: return "Unavailable"
        case .failed: return "Call failed"
        }
    }
}

/// The Recents list on disk, in the App Group container so both processes
/// reach it — but only the app writes the list. The extension leaves one
/// sidecar file per wake it reported (`pending/<call id>.json`), and the
/// app folds those in: a sidecar for a call the app has its own record of
/// is discarded, any other becomes a missed call. Two processes never
/// write one file.
public final class RecentsStore: @unchecked Sendable {
    public static let capacity = 500

    public let directory: URL
    private let lock = NSLock()
    private var listURL: URL { directory.appendingPathComponent("recents.json") }
    private var pendingDirectory: URL { directory.appendingPathComponent("pending", isDirectory: true) }

    /// A store rooted at `directory` (created if needed).
    public init(directory: URL) {
        self.directory = directory
        try? FileManager.default.createDirectory(at: pendingDirectory, withIntermediateDirectories: true)
    }

    /// The store in the App Group container (`<container>/recents/`), or
    /// nil when the container is unavailable (misconfigured entitlements).
    public convenience init?(appGroup: String = DiallerIDs.appGroup) {
        guard let base = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroup) else { return nil }
        self.init(directory: base.appendingPathComponent("recents", isDirectory: true))
    }

    private static func makeEncoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        return e
    }

    private static func makeDecoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .iso8601
        return d
    }

    // MARK: The list (app side)

    /// Every record, newest first. A missing or unreadable file is empty.
    public func load() -> [CallRecord] {
        lock.withLock { readLocked() }
    }

    /// Adds `record` at the top, replacing any earlier record of the same
    /// call (by either id), and trims to `capacity`. Returns the list.
    @discardableResult
    public func record(_ record: CallRecord) -> [CallRecord] {
        lock.withLock {
            var list = readLocked()
            list.removeAll { $0.matches(callID: record.id) || record.wakeCallID.map($0.matches(callID:)) == true }
            list.insert(record, at: 0)
            if list.count > Self.capacity { list.removeLast(list.count - Self.capacity) }
            writeLocked(list)
            return list
        }
    }

    @discardableResult
    public func delete(id: String) -> [CallRecord] {
        lock.withLock {
            var list = readLocked()
            list.removeAll { $0.id == id }
            writeLocked(list)
            return list
        }
    }

    public func clear() {
        lock.withLock { writeLocked([]) }
    }

    /// Folds the extension's sidecars into the list: one for a call already
    /// on the list is dropped, any other is adopted as it stands (a missed
    /// call, or answered elsewhere). Every sidecar is deleted. Returns the
    /// list, newest first.
    @discardableResult
    public func foldPending() -> [CallRecord] {
        lock.withLock {
            let fm = FileManager.default
            guard let files = try? fm.contentsOfDirectory(at: pendingDirectory, includingPropertiesForKeys: nil), !files.isEmpty else {
                return readLocked()
            }
            var list = readLocked()
            var adopted: [CallRecord] = []
            for url in files {
                defer { try? fm.removeItem(at: url) }
                guard let data = try? Data(contentsOf: url),
                      let r = try? Self.makeDecoder().decode(CallRecord.self, from: data) else { continue }
                let known = list.contains { $0.matches(callID: r.id) || r.wakeCallID.map($0.matches(callID:)) == true }
                if !known { adopted.append(r) }
            }
            guard !adopted.isEmpty else { return list }
            list.insert(contentsOf: adopted, at: 0)
            list.sort { $0.endedAt > $1.endedAt }
            if list.count > Self.capacity { list.removeLast(list.count - Self.capacity) }
            writeLocked(list)
            return list
        }
    }

    // MARK: Sidecars (extension side)

    /// The extension reported `wake` to the system: note a provisional
    /// missed call, which the app replaces with what actually happened —
    /// or keeps, if the app never ran for it.
    public func notePending(wake: Wake, at now: Date = Date()) {
        let r = CallRecord(id: wake.callID, direction: .incoming, counterpart: wake.from,
                           startedAt: now, endedAt: now, outcome: .missed)
        lock.withLock { writePendingLocked(r) }
    }

    /// The server withdrew a wake the extension reported: the provisional
    /// record ends now, answered elsewhere if that is why.
    public func notePending(cancel: WakeCancel, at now: Date = Date()) {
        lock.withLock {
            guard var r = readPendingLocked(callID: cancel.callID) else { return }
            r.endedAt = now
            if cancel.reason == .answeredElsewhere { r.outcome = .answeredElsewhere }
            writePendingLocked(r)
        }
    }

    /// The sidecars as they stand (tests and diagnostics).
    public func pending() -> [CallRecord] {
        lock.withLock {
            guard let files = try? FileManager.default.contentsOfDirectory(at: pendingDirectory, includingPropertiesForKeys: nil) else { return [] }
            return files.compactMap { url in
                (try? Data(contentsOf: url)).flatMap { try? Self.makeDecoder().decode(CallRecord.self, from: $0) }
            }
        }
    }

    // MARK: Files

    private func pendingURL(callID: String) -> URL {
        // Call ids are the server's (hex) or ours ("sip-3"); keep the file
        // name safe regardless.
        let safe = callID.map { $0.isLetter || $0.isNumber || $0 == "-" || $0 == "_" ? $0 : Character("_") }
        return pendingDirectory.appendingPathComponent(String(safe) + ".json")
    }

    private func readPendingLocked(callID: String) -> CallRecord? {
        (try? Data(contentsOf: pendingURL(callID: callID))).flatMap { try? Self.makeDecoder().decode(CallRecord.self, from: $0) }
    }

    private func writePendingLocked(_ r: CallRecord) {
        guard let data = try? Self.makeEncoder().encode(r) else { return }
        try? data.write(to: pendingURL(callID: r.id), options: .atomic)
    }

    private func readLocked() -> [CallRecord] {
        guard let data = try? Data(contentsOf: listURL),
              let list = try? Self.makeDecoder().decode([CallRecord].self, from: data) else { return [] }
        return list
    }

    private func writeLocked(_ list: [CallRecord]) {
        guard let data = try? Self.makeEncoder().encode(list) else { return }
        try? data.write(to: listURL, options: .atomic)
    }
}
