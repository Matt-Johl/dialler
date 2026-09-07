// swift-tools-version:5.9
import PackageDescription

// DiallerCore is the app logic that runs identically in the main app and the
// NEAppPushProvider extension (SPEC §2, §5): the foreground/extension TLS
// signal transport, the session state machine, enrolment + directory clients,
// and the incoming-call controller. Everything Apple-specific sits behind a
// protocol with a fake, so `swift test` covers it on macOS with no device.
let package = Package(
    name: "DiallerCore",
    platforms: [.iOS(.v17), .macOS(.v14)],
    products: [
        .library(name: "DiallerCore", targets: ["DiallerCore"]),
    ],
    dependencies: [
        .package(path: "../DiallerProtocol"),
    ],
    targets: [
        .target(name: "DiallerCore", dependencies: ["DiallerProtocol"]),
        .testTarget(name: "DiallerCoreTests", dependencies: ["DiallerCore"]),
    ]
)
