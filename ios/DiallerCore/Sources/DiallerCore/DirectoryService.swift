import Foundation

/// The directory the app reads and writes (SPEC §6 item 7): the server's
/// over HTTPS (`DirectoryClient`), or the demo's in memory (`DemoDirectory`,
/// appstore.md decision 2). The app syncs and writes through this and does
/// not know which it has.
public protocol DirectoryService: Sendable {
    /// Everything newer than `since` (0 = full sync, no tombstones).
    func changes(since: Int64) async throws -> DirectorySync
    /// Adds a contact. Returns it as stored.
    func create(_ draft: ContactDraft) async throws -> DirectoryContact
    func update(id: String, _ draft: ContactDraft) async throws -> DirectoryContact
    func delete(id: String) async throws
}

public extension DirectoryService {
    /// Brings `book` up to date: the delta from its cursor — or, when the
    /// server's version is *behind* the cursor, a reset and a full sync.
    /// A version can go backwards when the server's counter restarted: a
    /// re-provisioned harness, or the move to per-device directories
    /// (each starts at 1 while the app still held the old global cursor,
    /// 702 on 2026-09-21 — every write landed on the server and the app
    /// saw none of them). Returns true when it had to reset.
    @discardableResult
    func sync(_ book: inout AddressBook) async throws -> Bool {
        var delta = try await changes(since: book.version)
        var reset = false
        if delta.version < book.version {
            book.reset()
            delta = try await changes(since: 0)
            reset = true
        }
        book.apply(delta)
        return reset
    }
}
