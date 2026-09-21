import Foundation

/// What a QR code or a typed entry says about where to enrol (SPEC §4.8):
///
///	dialler://enrol?h=<host>&p=<https port>&c=<code>&f=<cert sha256, base64url>
///
/// Manual entry has no fingerprint: the claim reply supplies it, on trust
/// of the first connection.
public struct EnrolmentLink: Equatable, Sendable {
    public var host: String
    public var port: UInt16
    public var code: String
    public var certSHA256: String?

    public init(host: String, port: UInt16 = 8080, code: String, certSHA256: String? = nil) {
        self.host = host
        self.port = port
        self.code = code
        self.certSHA256 = certSHA256
    }

    /// Parses a `dialler://enrol` link; nil for anything else.
    public init?(url: URL) {
        guard url.scheme?.lowercased() == "dialler", url.host?.lowercased() == "enrol",
              let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems else { return nil }
        var q: [String: String] = [:]
        for i in items { q[i.name] = i.value }
        guard let h = q["h"], !h.isEmpty, let c = q["c"], !c.isEmpty else { return nil }
        host = h
        port = q["p"].flatMap(UInt16.init) ?? 8080
        code = Self.normalise(c)
        certSHA256 = q["f"].flatMap { $0.isEmpty ? nil : $0 }
    }

    /// The same normalisation the server applies: upper case, separators
    /// dropped, I and L to 1, O to 0. Done here too so what the user sees
    /// echoed back is what will be sent.
    public static func normalise(_ code: String) -> String {
        var out = ""
        for ch in code.uppercased() {
            switch ch {
            case " ", "-": continue
            case "I", "L": out.append("1")
            case "O": out.append("0")
            default: out.append(ch)
            }
        }
        return out
    }

    /// The claim endpoint.
    public var claimURL: URL { URL(string: "https://\(host):\(port)/v1/enrol")! }
}

/// The server's answer to a claim.
public struct EnrolmentResult: Codable, Equatable, Sendable {
    public var deviceID: String
    public var user: String
    public var token: String
    public var signalPort: Int
    public var sipDomain: String
    public var certSHA256: String

    enum CodingKeys: String, CodingKey {
        case user, token
        case deviceID = "device_id"
        case signalPort = "signal_port"
        case sipDomain = "sip_domain"
        case certSHA256 = "cert_sha256"
    }

    /// The configuration the app runs on from here: pinned to the
    /// certificate the server named, never "accept any".
    public func config(host: String, appVersion: String = "0.1.0") -> AppConfig {
        AppConfig(gateway: GatewayEndpoint(host: host, port: UInt16(clamping: signalPort), acceptAnyCertificate: false, certSHA256: certSHA256),
                  deviceID: deviceID, token: token, appVersion: appVersion)
    }
}

public enum EnrolmentError: LocalizedError, Equatable {
    /// 404: unknown, used or expired.
    case badCode
    /// 429: the server is refusing this address for a minute.
    case tooManyAttempts
    /// The server could not be reached, or answered something else.
    case server(String)
    /// Manual entry: the certificate the server presented is not the one
    /// its reply named — someone is between the phone and the server.
    case certificateMismatch

    public var errorDescription: String? {
        switch self {
        case .badCode: return "That code is not valid, has been used, or has expired. Ask for a new one."
        case .tooManyAttempts: return "Too many attempts. Wait a minute and try again."
        case .server(let s): return "Could not reach the server: \(s)"
        case .certificateMismatch: return "The server's certificate does not match its enrolment reply. Do not continue on this network."
        }
    }
}

/// Claims an enrolment code (SPEC §6 item 8). With a fingerprint from the
/// QR the connection is pinned from the start; without one (typed entry)
/// the first connection is trusted, and the certificate it presented must
/// be the one the reply names.
public struct EnrolmentClient {
    private let makeSession: (String?) -> (URLSession, EndpointTrust?)

    public init() {
        makeSession = { pin in
            let trust = EndpointTrust(pin: pin, acceptAny: pin == nil)
            return (URLSession(configuration: .default, delegate: trust, delegateQueue: nil), trust)
        }
    }

    /// For tests: a session of the caller's choosing (no trust evaluation).
    public init(session: URLSession) {
        makeSession = { _ in (session, nil) }
    }

    public func claim(_ link: EnrolmentLink) async throws -> EnrolmentResult {
        let (session, trust) = makeSession(link.certSHA256)
        var req = URLRequest(url: link.claimURL)
        req.httpMethod = "POST"
        req.timeoutInterval = 15
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONEncoder().encode(["code": link.code])
        let data: Data
        let resp: URLResponse
        do {
            (data, resp) = try await session.data(for: req)
        } catch {
            throw EnrolmentError.server(error.localizedDescription)
        }
        guard let http = resp as? HTTPURLResponse else { throw EnrolmentError.server("no HTTP response") }
        switch http.statusCode {
        case 200: break
        case 404: throw EnrolmentError.badCode
        case 429: throw EnrolmentError.tooManyAttempts
        default: throw EnrolmentError.server("HTTP \(http.statusCode)")
        }
        let result: EnrolmentResult
        do {
            result = try JSONDecoder().decode(EnrolmentResult.self, from: data)
        } catch {
            throw EnrolmentError.server("unreadable reply")
        }
        // Trust on first use: the certificate we just talked to must be the
        // one the server says to pin. With a QR fingerprint this held
        // already (the session would have refused otherwise).
        if let seen = trust?.seenFingerprint, seen != result.certSHA256 {
            throw EnrolmentError.certificateMismatch
        }
        return result
    }
}
