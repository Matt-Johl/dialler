import Foundation

/// The demo's directory (see `Demo`): in memory, versioned like the server's
/// (server/internal/directory), so sync, add, edit, delete and favourites
/// behave exactly as they do against a real server. Starts from
/// `Demo.contacts` each time demo mode starts.
public actor DemoDirectory: DirectoryService {
    public enum Failure: Error { case notFound }

    private var version: Int64 = 0
    private var contacts: [String: DirectoryContact] = [:]
    private var nextID = 1

    public init(seed: [Demo.Contact] = Demo.contacts) {
        for c in seed {
            version += 1
            let id = "demo-\(nextID)"
            nextID += 1
            contacts[id] = DirectoryContact(id: id, displayName: c.name, uri: Demo.uri(c.number), mode: c.mode,
                                            version: version, favourite: c.favourite ? true : nil)
        }
    }

    public func changes(since: Int64) -> DirectorySync {
        // A full sync (since 0) carries no tombstones, as on the server.
        let changed = contacts.values
            .filter { $0.version > since && (since > 0 || $0.deleted != true) }
            .sorted { $0.version < $1.version }
        return DirectorySync(version: version, since: since, contacts: changed)
    }

    public func create(_ draft: ContactDraft) -> DirectoryContact {
        version += 1
        let id = "demo-\(nextID)"
        nextID += 1
        let c = contact(id: id, draft)
        contacts[id] = c
        return c
    }

    public func update(id: String, _ draft: ContactDraft) throws -> DirectoryContact {
        guard let old = contacts[id], old.deleted != true else { throw Failure.notFound }
        version += 1
        let c = contact(id: id, draft)
        contacts[id] = c
        return c
    }

    public func delete(id: String) throws {
        guard var c = contacts[id], c.deleted != true else { throw Failure.notFound }
        version += 1
        c.version = version
        c.deleted = true
        contacts[id] = c
    }

    private func contact(id: String, _ draft: ContactDraft) -> DirectoryContact {
        DirectoryContact(id: id, displayName: draft.displayName, uri: draft.uri, mode: draft.mode,
                         version: version, favourite: draft.favourite ? true : nil)
    }
}
