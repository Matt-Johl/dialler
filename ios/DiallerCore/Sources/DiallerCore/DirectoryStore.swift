import Foundation

/// The address book on disk (SPEC §6 item 7): `<App Group>/directory/book.json`,
/// so the sync cursor survives a launch and the list is there before the
/// first sync of the session. One file, written atomically, by the app only.
public final class AddressBookStore: @unchecked Sendable {
    public let url: URL
    private let lock = NSLock()

    public init(url: URL) {
        self.url = url
    }

    /// The store in the App Group container, or nil when the container is
    /// unavailable (misconfigured entitlements).
    public convenience init?(appGroup: String = DiallerIDs.appGroup) {
        guard let base = FileManager.default.containerURL(forSecurityApplicationGroupIdentifier: appGroup) else { return nil }
        let dir = base.appendingPathComponent("directory", isDirectory: true)
        try? FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        self.init(url: dir.appendingPathComponent("book.json"))
    }

    /// The saved book, or an empty one (version 0: the next sync is full).
    public func load() -> AddressBook {
        lock.withLock {
            guard let data = try? Data(contentsOf: url),
                  let book = try? JSONDecoder().decode(AddressBook.self, from: data) else { return AddressBook() }
            return book
        }
    }

    public func save(_ book: AddressBook) {
        lock.withLock {
            guard let data = try? JSONEncoder().encode(book) else { return }
            try? data.write(to: url, options: .atomic)
        }
    }

    public func clear() {
        lock.withLock { try? FileManager.default.removeItem(at: url) }
    }
}

/// The Directory tab's search (SPEC §6 item 7): any substring of the name
/// or of the number, case- and diacritic-insensitive, applied locally so
/// it is instant and works offline. Kept here so it is tested.
public enum DirectorySearch {
    /// Whether `contact` matches `query`; an empty or blank query matches all.
    public static func matches(_ contact: DirectoryContact, query: String) -> Bool {
        let q = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !q.isEmpty else { return true }
        let options: String.CompareOptions = [.caseInsensitive, .diacriticInsensitive]
        if contact.displayName.range(of: q, options: options) != nil { return true }
        let number = CallController.numberPart(of: contact.uri)
        return number.range(of: q, options: options) != nil
    }

    public static func filter(_ contacts: [DirectoryContact], query: String) -> [DirectoryContact] {
        contacts.filter { matches($0, query: query) }
    }
}
