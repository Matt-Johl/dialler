import CryptoKit
import Foundation
import Security

/// Certificate pinning after enrolment (SPEC §4.8). The QR link — or, on
/// the manual path, the claim reply — carries the server certificate's
/// SHA-256, and from then on the signal socket and every HTTPS call accept
/// that certificate and no other. It replaces "accept any certificate",
/// which survives only as the dev toggle on the Status page.
public enum CertificatePin {
    /// The base64url SHA-256 of a certificate's DER, as the server prints it.
    public static func fingerprint(der: Data) -> String {
        Data(SHA256.hash(data: der)).base64URLEncodedString()
    }

    public static func fingerprint(of certificate: SecCertificate) -> String {
        fingerprint(der: SecCertificateCopyData(certificate) as Data)
    }

    /// The fingerprint of the leaf of a server's presented chain.
    public static func leafFingerprint(of trust: SecTrust) -> String? {
        guard let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate], let leaf = chain.first else { return nil }
        return fingerprint(of: leaf)
    }

    /// Whether the presented chain's leaf is the pinned certificate.
    public static func matches(_ trust: SecTrust, pin: String) -> Bool {
        guard let got = leafFingerprint(of: trust) else { return false }
        return got == pin
    }
}

extension Data {
    func base64URLEncodedString() -> String {
        base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }
}

/// URLSession trust for a `GatewayEndpoint`: pinned when it has a
/// fingerprint, any certificate when the dev toggle is on, otherwise the
/// system's roots. Also records the leaf it saw, for trust-on-first-use.
public final class EndpointTrust: NSObject, URLSessionDelegate, @unchecked Sendable {
    private let pin: String?
    private let acceptAny: Bool
    private let lock = NSLock()
    private var _seen: String?

    public init(pin: String?, acceptAny: Bool) {
        self.pin = pin
        self.acceptAny = acceptAny
    }

    /// The fingerprint of the last server certificate presented.
    public var seenFingerprint: String? { lock.withLock { _seen } }

    public func urlSession(_ session: URLSession, didReceive challenge: URLAuthenticationChallenge,
                           completionHandler: @escaping (URLSession.AuthChallengeDisposition, URLCredential?) -> Void) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let trust = challenge.protectionSpace.serverTrust else {
            completionHandler(.performDefaultHandling, nil)
            return
        }
        let seen = CertificatePin.leafFingerprint(of: trust)
        lock.withLock { _seen = seen }
        if let pin {
            completionHandler(seen == pin ? .useCredential : .cancelAuthenticationChallenge, seen == pin ? URLCredential(trust: trust) : nil)
        } else if acceptAny {
            completionHandler(.useCredential, URLCredential(trust: trust))
        } else {
            completionHandler(.performDefaultHandling, nil)
        }
    }
}

public extension URLSession {
    /// A session trusting `endpoint`'s server as configured.
    static func forEndpoint(pin: String?, acceptAny: Bool) -> URLSession {
        guard pin != nil || acceptAny else { return .shared }
        return URLSession(configuration: .default, delegate: EndpointTrust(pin: pin, acceptAny: acceptAny), delegateQueue: nil)
    }
}
