import Foundation

// Typed message bodies for protocol v1. Field names follow PROTOCOL.md
// exactly; CodingKeys are explicit so the JSON never drifts with Swift
// naming.

public enum ClientKind: String, Codable, Sendable {
    case app
    case extensionKind = "extension"
}

public struct Hello: Codable, Equatable, Sendable {
    public var deviceID: String
    public var token: String
    public var client: ClientKind
    public var appVersion: String?
    public var capabilities: [String]?

    public init(deviceID: String, token: String, client: ClientKind, appVersion: String? = nil, capabilities: [String]? = nil) {
        self.deviceID = deviceID
        self.token = token
        self.client = client
        self.appVersion = appVersion
        self.capabilities = capabilities
    }

    enum CodingKeys: String, CodingKey {
        case deviceID = "device_id", token, client, appVersion = "app_version", capabilities
    }
}

/// How a connected app should register its SIP user agent so calls reach
/// it directly while it runs (PROTOCOL.md §3, additive field on welcome).
public struct SIPAccount: Codable, Equatable, Sendable {
    public var user: String
    public var domain: String
    public var host: String
    public var port: Int
    public var transport: String

    public init(user: String, domain: String, host: String, port: Int, transport: String) {
        self.user = user
        self.domain = domain
        self.host = host
        self.port = port
        self.transport = transport
    }
}

public struct Welcome: Codable, Equatable, Sendable {
    public var sessionID: String
    public var heartbeatSeconds: Int
    public var serverTime: Date
    public var directoryVersion: Int64
    public var sip: SIPAccount?

    public init(sessionID: String, heartbeatSeconds: Int, serverTime: Date, directoryVersion: Int64, sip: SIPAccount? = nil) {
        self.sessionID = sessionID
        self.heartbeatSeconds = heartbeatSeconds
        self.serverTime = serverTime
        self.directoryVersion = directoryVersion
        self.sip = sip
    }

    enum CodingKeys: String, CodingKey {
        case sessionID = "session_id", heartbeatSeconds = "heartbeat_seconds"
        case serverTime = "server_time", directoryVersion = "directory_version", sip
    }
}

public struct Party: Codable, Equatable, Sendable {
    public var displayName: String?
    public var uri: String

    public init(displayName: String? = nil, uri: String) {
        self.displayName = displayName
        self.uri = uri
    }

    enum CodingKeys: String, CodingKey { case displayName = "display_name", uri }
}

public struct SIPTarget: Codable, Equatable, Sendable {
    public var host: String
    public var port: Int
    public var transport: String

    public init(host: String, port: Int, transport: String) {
        self.host = host
        self.port = port
        self.transport = transport
    }
}

public struct Wake: Codable, Equatable, Sendable {
    public var callID: String
    public var from: Party
    public var to: Party
    public var sip: SIPTarget
    public var expiresAt: Date

    public init(callID: String, from: Party, to: Party, sip: SIPTarget, expiresAt: Date) {
        self.callID = callID
        self.from = from
        self.to = to
        self.sip = sip
        self.expiresAt = expiresAt
    }

    enum CodingKeys: String, CodingKey {
        case callID = "call_id", from, to, sip, expiresAt = "expires_at"
    }
}

public enum WakeAction: String, Codable, Sendable {
    case willAnswer = "will_answer"
    case decline
    case busy
}

public struct WakeAck: Codable, Equatable, Sendable {
    public var callID: String
    public var action: WakeAction

    public init(callID: String, action: WakeAction) {
        self.callID = callID
        self.action = action
    }

    enum CodingKeys: String, CodingKey { case callID = "call_id", action }
}

public enum CancelReason: String, Codable, Sendable {
    case callerHangup = "caller_hangup"
    case answeredElsewhere = "answered_elsewhere"
    case timeout
}

public struct WakeCancel: Codable, Equatable, Sendable {
    public var callID: String
    public var reason: CancelReason

    public init(callID: String, reason: CancelReason) {
        self.callID = callID
        self.reason = reason
    }

    enum CodingKeys: String, CodingKey { case callID = "call_id", reason }
}

public struct DirectoryChanged: Codable, Equatable, Sendable {
    public var version: Int64

    public init(version: Int64) { self.version = version }
}

public enum ErrorCode: String, Codable, Sendable {
    case unsupportedVersion = "unsupported_version"
    case badFrame = "bad_frame"
    case helloExpected = "hello_expected"
    case unauthorized
    case superseded
    case idleTimeout = "idle_timeout"
    case unknownCall = "unknown_call"
}

public struct ProtocolError: Codable, Equatable, Sendable {
    public var code: ErrorCode
    public var message: String?
    public var fatal: Bool

    public init(code: ErrorCode, message: String? = nil, fatal: Bool) {
        self.code = code
        self.message = message
        self.fatal = fatal
    }
}
