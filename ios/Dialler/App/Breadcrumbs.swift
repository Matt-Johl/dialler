import DiallerCore
import Foundation
import os

/// The app's persistent log (App Group, uploaded to the dev server) and a
/// breadcrumb writer for the launch path. A launch that hangs in a system
/// call before the app's own code runs (CallKit provider registration, the
/// audio session, Local Push) used to leave no trace at all; now the last
/// breadcrumb before the silence names the step.
enum Breadcrumb {
    static let log = FileLog(name: "app")
    /// Mirrored to the unified log too, so a device log archive shows the
    /// app's launch steps in line with the system's own messages (which
    /// process was launched, by whom, and what killed it).
    private static let logger = Logger(subsystem: DiallerIDs.bundlePrefix, category: "launch")

    static func drop(_ step: String) {
        logger.notice("\(step, privacy: .public)")
        log?.write("launch: \(step)")
    }
}
