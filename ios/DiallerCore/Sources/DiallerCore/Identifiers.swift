import Foundation

/// Identifiers shared by the app and the extension. Change these in one
/// place (and in the Xcode targets' bundle ids / entitlements) when signing
/// for a different team.
public enum DiallerIDs {
    public static let bundlePrefix = "com.latentbadger.dialler"
    public static let appGroup = "group.com.latentbadger.dialler"
    public static let pushProviderBundleID = bundlePrefix + ".PushProvider"
}
