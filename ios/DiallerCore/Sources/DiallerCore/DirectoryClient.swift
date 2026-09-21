import Foundation

/// One address-book entry as served by GET /v1/directory.
public struct DirectoryContact: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var displayName: String
    public var uri: String
    public var mode: String // "local" | "trunk"
    public var version: Int64
    public var deleted: Bool?
    /// Starred by the user (or the admin); the server omits it when false.
    public var favourite: Bool?

    public init(id: String, displayName: String, uri: String, mode: String, version: Int64, deleted: Bool? = nil, favourite: Bool? = nil) {
        self.id = id
        self.displayName = displayName
        self.uri = uri
        self.mode = mode
        self.version = version
        self.deleted = deleted
        self.favourite = favourite
    }

    public var isFavourite: Bool { favourite == true }

    enum CodingKeys: String, CodingKey {
        case id, uri, mode, version, deleted, favourite
        case displayName = "display_name"
    }
}

/// What the app sends to create or change a contact: the fields the user
/// owns. The server assigns id and version.
public struct ContactDraft: Codable, Equatable, Sendable {
    public var displayName: String
    public var uri: String
    public var mode: String
    public var favourite: Bool

    public init(displayName: String, uri: String, mode: String, favourite: Bool = false) {
        self.displayName = displayName
        self.uri = uri
        self.mode = mode
        self.favourite = favourite
    }

    public init(_ c: DirectoryContact) {
        self.init(displayName: c.displayName, uri: c.uri, mode: c.mode, favourite: c.isFavourite)
    }

    enum CodingKeys: String, CodingKey {
        case uri, mode, favourite
        case displayName = "display_name"
    }
}

public struct DirectorySync: Codable, Equatable, Sendable {
    public var version: Int64
    public var since: Int64
    public var contacts: [DirectoryContact]
}

/// Delta-sync client for the directory API (SPEC §5 component 8, server
/// side in server/internal/directory). Device-authenticated with the same
/// credential as the gateway.
public struct DirectoryClient {
    private let base: URL
    private let deviceID: String
    private let token: String
    private let session: URLSession

    public init(base: URL, deviceID: String, token: String, session: URLSession = .shared) {
        self.base = base
        self.deviceID = deviceID
        self.token = token
        self.session = session
    }

    /// Fetch everything newer than `since` (0 = full sync, no tombstones).
    public func changes(since: Int64) async throws -> DirectorySync {
        let req = request("GET", path: "v1/directory", query: [.init(name: "since", value: String(since))])
        return try JSONDecoder().decode(DirectorySync.self, from: try await perform(req))
    }

    // MARK: Writes (SPEC §6 item 7): the device's own directory, online only.

    /// Adds a contact (the server matches an existing one by URI). Returns
    /// it as stored; sync afterwards to pick up any tombstones the collapse
    /// of duplicates produced.
    public func create(_ draft: ContactDraft) async throws -> DirectoryContact {
        var req = request("POST", path: "v1/directory")
        req.httpBody = try JSONEncoder().encode(draft)
        return try JSONDecoder().decode(DirectoryContact.self, from: try await perform(req))
    }

    public func update(id: String, _ draft: ContactDraft) async throws -> DirectoryContact {
        var req = request("PUT", path: "v1/directory/\(id)")
        req.httpBody = try JSONEncoder().encode(draft)
        return try JSONDecoder().decode(DirectoryContact.self, from: try await perform(req))
    }

    public func delete(id: String) async throws {
        _ = try await perform(request("DELETE", path: "v1/directory/\(id)"))
    }

    private func request(_ method: String, path: String, query: [URLQueryItem] = []) -> URLRequest {
        var url = base.appendingPathComponent(path)
        if !query.isEmpty { url = url.appending(queryItems: query) }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.timeoutInterval = 15
        req.setValue(deviceID, forHTTPHeaderField: "X-Device-ID")
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if method != "GET" { req.setValue("application/json", forHTTPHeaderField: "Content-Type") }
        return req
    }

    private func perform(_ req: URLRequest) async throws -> Data {
        let (data, resp) = try await session.data(for: req)
        guard let http = resp as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
            throw URLError(.badServerResponse)
        }
        return data
    }
}

public extension DirectoryClient {
    /// A session for the directory API over TLS. The server's certificate is
    /// self-signed in development; `acceptAnyCertificate` trusts it, the
    /// same switch the gateway transport honours. Off, the system trust
    /// store applies.
    static func session(acceptAnyCertificate: Bool) -> URLSession {
        guard acceptAnyCertificate else { return .shared }
        return URLSession(configuration: .default, delegate: AnyCertificateTrust(), delegateQueue: nil)
    }
}

/// Accepts any server certificate. Development only.
final class AnyCertificateTrust: NSObject, URLSessionDelegate {
    func urlSession(_ session: URLSession, didReceive challenge: URLAuthenticationChallenge,
                    completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
        if challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
           let trust = challenge.protectionSpace.serverTrust {
            completionHandler(.useCredential, URLCredential(trust: trust))
        } else {
            completionHandler(.performDefaultHandling, nil)
        }
    }
}

/// Thread-safe lookup of a caller's friendly name in the directory, for
/// naming an incoming call. The call controller resolves names on whatever
/// thread a wake arrives on, not the main actor, so this holds its own
/// snapshot behind a lock rather than reading the app's @Published contacts.
///
/// A caller URI is matched first exactly, then by the user before the "@"
/// (a trunk caller arrives as sip:101@<pbx-ip> but the directory entry is
/// sip:101@asterisk — same extension, different host).
public final class DirectoryNameIndex: @unchecked Sendable {
    private let lock = NSLock()
    private var byURI: [String: String] = [:]
    private var byUser: [String: String] = [:]

    public init() {}

    public func update(_ contacts: [DirectoryContact]) {
        var uris: [String: String] = [:]
        var users: [String: String] = [:]
        for c in contacts {
            uris[c.uri.lowercased()] = c.displayName
            let u = Self.user(of: c.uri)
            if !u.isEmpty { users[u] = c.displayName }
        }
        lock.lock(); byURI = uris; byUser = users; lock.unlock()
    }

    public func name(forURI uri: String) -> String? {
        let u = Self.user(of: uri)
        lock.lock(); defer { lock.unlock() }
        return byURI[uri.lowercased()] ?? (u.isEmpty ? nil : byUser[u])
    }

    /// "sip:101@10.18.0.5;transport=udp" → "101"; "201@dialler" → "201".
    static func user(of uri: String) -> String {
        CallController.numberPart(of: uri).lowercased()
    }
}

/// Local address book with reconcile: applies deltas (including tombstones)
/// and remembers the cursor. Persisted by `AddressBookStore`.
public struct AddressBook: Codable, Equatable, Sendable {
    public private(set) var version: Int64 = 0
    public private(set) var contacts: [String: DirectoryContact] = [:]

    public init() {}

    public var sorted: [DirectoryContact] {
        contacts.values.sorted { $0.displayName.localizedCaseInsensitiveCompare($1.displayName) == .orderedAscending }
    }

    /// The starred contacts, in the same order.
    public var favourites: [DirectoryContact] { sorted.filter(\.isFavourite) }

    public mutating func apply(_ sync: DirectorySync) {
        for c in sync.contacts {
            if c.deleted == true {
                contacts[c.id] = nil
            } else {
                contacts[c.id] = c
            }
        }
        version = max(version, sync.version)
    }

    /// An optimistic local change (a star toggled) shown before the server
    /// confirms it; the next sync brings the truth either way.
    public mutating func setFavourite(id: String, _ on: Bool) {
        contacts[id]?.favourite = on ? true : nil
    }

    /// Forget everything: the next sync starts from version 0.
    public mutating func reset() {
        version = 0
        contacts = [:]
    }
}
