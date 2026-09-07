import Foundation

/// Errors raised while decoding or framing.
public enum WireError: Error, Equatable, Sendable {
    case unsupportedVersion(Int)
    case missingID
    case missingType
    case frameTooLarge(Int)
}

/// A typed v1 message. Unknown types decode to `.unknown` and are ignored
/// by receivers (PROTOCOL.md §2) so the server can add types without
/// breaking shipped clients.
public enum Message: Equatable, Sendable {
    case hello(Hello)
    case welcome(Welcome)
    case ping
    case pong
    case wake(Wake)
    case wakeAck(WakeAck)
    case wakeCancel(WakeCancel)
    case directoryChanged(DirectoryChanged)
    case error(ProtocolError)
    case unknown(type: String)

    /// The `type` string on the wire.
    public var typeName: String {
        switch self {
        case .hello: return "hello"
        case .welcome: return "welcome"
        case .ping: return "ping"
        case .pong: return "pong"
        case .wake: return "wake"
        case .wakeAck: return "wake_ack"
        case .wakeCancel: return "wake_cancel"
        case .directoryChanged: return "directory_changed"
        case .error: return "error"
        case .unknown(let t): return t
        }
    }
}

/// The outer JSON object carried in every frame.
public struct Envelope: Codable, Equatable, Sendable {
    public static let version = 1

    public var id: String
    public var ts: Date
    public var message: Message

    public init(id: String, ts: Date, message: Message) {
        self.id = id
        self.ts = ts
        self.message = message
    }

    enum CodingKeys: String, CodingKey { case v, type, id, ts, body }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let v = try c.decode(Int.self, forKey: .v)
        guard v == Envelope.version else { throw WireError.unsupportedVersion(v) }
        let type = try c.decode(String.self, forKey: .type)
        guard !type.isEmpty else { throw WireError.missingType }
        id = try c.decode(String.self, forKey: .id)
        guard !id.isEmpty else { throw WireError.missingID }
        ts = try c.decode(Date.self, forKey: .ts)

        switch type {
        case "hello": message = .hello(try c.decode(Hello.self, forKey: .body))
        case "welcome": message = .welcome(try c.decode(Welcome.self, forKey: .body))
        case "ping": message = .ping
        case "pong": message = .pong
        case "wake": message = .wake(try c.decode(Wake.self, forKey: .body))
        case "wake_ack": message = .wakeAck(try c.decode(WakeAck.self, forKey: .body))
        case "wake_cancel": message = .wakeCancel(try c.decode(WakeCancel.self, forKey: .body))
        case "directory_changed": message = .directoryChanged(try c.decode(DirectoryChanged.self, forKey: .body))
        case "error": message = .error(try c.decode(ProtocolError.self, forKey: .body))
        default: message = .unknown(type: type)
        }
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(Envelope.version, forKey: .v)
        try c.encode(message.typeName, forKey: .type)
        try c.encode(id, forKey: .id)
        try c.encode(ts, forKey: .ts)
        switch message {
        case .hello(let b): try c.encode(b, forKey: .body)
        case .welcome(let b): try c.encode(b, forKey: .body)
        case .wake(let b): try c.encode(b, forKey: .body)
        case .wakeAck(let b): try c.encode(b, forKey: .body)
        case .wakeCancel(let b): try c.encode(b, forKey: .body)
        case .directoryChanged(let b): try c.encode(b, forKey: .body)
        case .error(let b): try c.encode(b, forKey: .body)
        case .ping, .pong, .unknown: break
        }
    }
}

/// JSON coders configured for the wire format (RFC 3339 timestamps).
public enum WireCoding {
    public static func makeDecoder() -> JSONDecoder {
        let d = JSONDecoder()
        // The server (Go) writes RFC 3339 with nanoseconds. Foundation's
        // built-in .iso8601 strategy rejects fractional seconds on older
        // runtimes (iOS 18 simulator) while accepting them on newer ones;
        // accept both explicitly so the protocol does not depend on that.
        d.dateDecodingStrategy = .custom { decoder in
            let s = try decoder.singleValueContainer().decode(String.self)
            if let date = Self.fractional.date(from: s) ?? Self.whole.date(from: s) { return date }
            throw DecodingError.dataCorrupted(.init(codingPath: decoder.codingPath, debugDescription: "not an RFC 3339 timestamp: \(s)"))
        }
        return d
    }

    /// ISO8601DateFormatter is thread-safe; nanosecond digits beyond three
    /// are still parsed (the formatter truncates).
    private static let fractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    private static let whole: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()

    public static func makeEncoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.dateEncodingStrategy = .iso8601
        return e
    }

    public static func decode(_ data: Data) throws -> Envelope {
        try makeDecoder().decode(Envelope.self, from: data)
    }

    public static func encode(_ envelope: Envelope) throws -> Data {
        try makeEncoder().encode(envelope)
    }
}
