// swift-tools-version:5.9
import PackageDescription

// DiallerProtocol is the App Group shared framework's protocol layer: the
// v1 wire envelope, typed bodies, and TLS frame codec. It is used unchanged
// by the main app's foreground transport and the NEAppPushProvider extension
// (SPEC §2), and must round-trip every fixture in protocol/fixtures.
let package = Package(
    name: "DiallerProtocol",
    platforms: [.iOS(.v16), .macOS(.v13)],
    products: [
        .library(name: "DiallerProtocol", targets: ["DiallerProtocol"]),
    ],
    targets: [
        .target(name: "DiallerProtocol"),
        .testTarget(name: "DiallerProtocolTests", dependencies: ["DiallerProtocol"]),
    ]
)
