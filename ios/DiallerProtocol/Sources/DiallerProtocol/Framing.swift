import Foundation

/// Length-prefixed framing for the TLS stream (PROTOCOL.md §1):
/// `uint32` big-endian payload length followed by the JSON payload.
public enum Frame {
    public static let maxPayload = 64 * 1024

    /// Wraps one payload in a frame.
    public static func encode(_ payload: Data) throws -> Data {
        guard payload.count <= maxPayload else { throw WireError.frameTooLarge(payload.count) }
        var out = Data(capacity: 4 + payload.count)
        var len = UInt32(payload.count).bigEndian
        withUnsafeBytes(of: &len) { out.append(contentsOf: $0) }
        out.append(payload)
        return out
    }

    /// Encodes an envelope straight to a frame.
    public static func encode(_ envelope: Envelope) throws -> Data {
        try encode(WireCoding.encode(envelope))
    }
}

/// Incremental frame parser for a byte stream. Feed it whatever the socket
/// delivers; pull complete payloads with `next()`.
public struct FrameDecoder: Sendable {
    private var buffer = Data()

    public init() {}

    public mutating func append(_ data: Data) {
        buffer.append(data)
    }

    /// Returns the next complete payload, or nil if more bytes are needed.
    /// Throws `frameTooLarge` on an oversized length prefix; the caller must
    /// then close the connection (the stream is unrecoverable).
    public mutating func next() throws -> Data? {
        guard buffer.count >= 4 else { return nil }
        let len = buffer.prefix(4).reduce(0) { ($0 << 8) | Int($1) }
        guard len <= Frame.maxPayload else { throw WireError.frameTooLarge(len) }
        guard buffer.count >= 4 + len else { return nil }
        let payload = buffer.subdata(in: 4 ..< 4 + len)
        buffer.removeSubrange(0 ..< 4 + len)
        return payload
    }

    /// Decodes the next complete envelope, or nil if more bytes are needed.
    public mutating func nextEnvelope() throws -> Envelope? {
        guard let payload = try next() else { return nil }
        return try WireCoding.decode(payload)
    }
}
