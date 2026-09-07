import DiallerCore
import DiallerProtocol
import PushKit
import SwiftUI
import UIKit

@main
struct DiallerApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        WindowGroup {
            ContentView().environmentObject(delegate.model)
        }
    }
}

/// Owns the long-lived objects and receives PushKit deliveries from the
/// NEAppPushProvider extension (Local Push Connectivity surfaces incoming
/// calls to the app through the VoIP push registry).
final class AppDelegate: NSObject, UIApplicationDelegate, PKPushRegistryDelegate {
    /// Created inside didFinishLaunching (first touch below), not while the
    /// delegate itself is being instantiated: the CXProvider it owns is then
    /// registered with CallKit at the same point Apple's samples do it.
    lazy var model = AppModel()
    private var pushRegistry: PKPushRegistry?

    func application(_ application: UIApplication, didFinishLaunchingWithOptions _: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        let registry = PKPushRegistry(queue: .main)
        registry.delegate = self
        registry.desiredPushTypes = [.voIP]
        pushRegistry = registry
        model.autoConnectIfConfigured()
        return true
    }

    func pushRegistry(_: PKPushRegistry, didUpdate _: PKPushCredentials, for _: PKPushType) {
        // APNS credentials are not used in the LPC-only phase (SPEC §2).
    }

    func pushRegistry(_: PKPushRegistry, didReceiveIncomingPushWith payload: PKPushPayload, for _: PKPushType, completion: @escaping () -> Void) {
        // iOS requires CallKit to be reported synchronously from this callback.
        model.handleExtensionWake(userInfo: payload.dictionaryPayload)
        completion()
    }
}
