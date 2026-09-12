// swift-tools-version:5.9
import PackageDescription

// DiallerEngine: the baresip-backed CallEngine (SPEC §3 SIP + media stack).
// The C stack (libre, baresip with static modules, Opus, OpenSSL) comes from
// ios/vendor/build-baresip.sh as XCFrameworks; CBaresip is a small C shim
// that owns the libre thread and exposes a Swift-friendly surface.
let package = Package(
    name: "DiallerEngine",
    platforms: [.iOS(.v17), .macOS(.v14)], // macOS: headless engine-probe against the docker server
    products: [
        .library(name: "DiallerEngine", targets: ["DiallerEngine"]),
        .executable(name: "engine-probe", targets: ["engine-probe"]),
        .executable(name: "audio-probe", targets: ["audio-probe"]),
        .executable(name: "sim-call", targets: ["sim-call"]),
    ],
    dependencies: [
        .package(path: "../DiallerCore"),
        .package(path: "../DiallerProtocol"),
    ],
    targets: [
        .binaryTarget(name: "re", path: "../vendor/xcframeworks/re.xcframework"),
        .binaryTarget(name: "baresip", path: "../vendor/xcframeworks/baresip.xcframework"),
        .binaryTarget(name: "opus", path: "../vendor/xcframeworks/opus.xcframework"),
        .binaryTarget(name: "ssl", path: "../vendor/xcframeworks/ssl.xcframework"),
        .binaryTarget(name: "crypto", path: "../vendor/xcframeworks/crypto.xcframework"),
        .target(
            name: "CBaresip",
            dependencies: ["re", "baresip", "opus", "ssl", "crypto"],
            linkerSettings: [
                .linkedFramework("AudioToolbox"),
                .linkedFramework("AVFoundation"),
                .linkedFramework("CoreAudio"),
                .linkedFramework("Security"),
                .linkedLibrary("z"),
                .linkedLibrary("resolv"), // libre's DNS client (res_ninit & co.)
                .linkedFramework("SystemConfiguration"), // libre's DNS server discovery on macOS
            ]
        ),
        .target(name: "DiallerEngine", dependencies: ["CBaresip", "DiallerCore", "DiallerProtocol"]),
        .executableTarget(name: "engine-probe", dependencies: ["DiallerEngine", "DiallerCore", "DiallerProtocol"]),
        // Drives the audiounit driver directly (no SIP) through the CallKit
        // orderings and asserts frames flow. Runs on macOS and, via
        // `simctl spawn`, on the iOS simulator (VoiceProcessingIO).
        .executableTarget(name: "audio-probe", dependencies: ["CBaresip"]),
        // The app's call path headless (gateway session, wake, register,
        // answer, CallKit activation sequence) on the iOS simulator against
        // the docker harness, asserted on RTP + rendered audio energy.
        .executableTarget(name: "sim-call", dependencies: ["DiallerEngine", "DiallerCore", "DiallerProtocol", "CBaresip"]),
        // Pure-Swift checks on the engine (the baresip config profile); no
        // SIP stack is started. `cd ios/DiallerEngine && swift test --disable-sandbox`.
        .testTarget(name: "DiallerEngineTests", dependencies: ["DiallerEngine"]),
    ]
)
