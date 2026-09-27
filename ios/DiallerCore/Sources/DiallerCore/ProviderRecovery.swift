import Foundation

/// What the CallKit bridge does about its provider's registration with the
/// system, as a pure policy so the device-only history can be pinned by
/// tests.
///
/// Background (dev-a, 2026-09-13 → 2026-09-27, nine resets): CallKit calls
/// `providerDidReset` when the provider's connection to callservicesd was
/// interrupted, which on this phone only ever happened while the app was
/// suspended; the reset is delivered on resume, always in the background.
/// When the app became active within ~200 ms of the reset (the user opened
/// it), the registration came back by itself and the next outgoing call was
/// accepted in 20 ms. On 2026-09-27 10:41:54Z the app stayed in the
/// background 2.8 s after the reset, and its registration never returned:
/// every `CXStartCallAction` for the next 90 s was answered by the system
/// with `unknownCallProvider` after a 10 s wait, without ever reaching the
/// provider. The registration came back only when the app next called INTO
/// the provider (`reportNewIncomingCall` at 10:43:31Z), and fully only on
/// relaunch, which builds a new `CXProvider`.
///
/// So: a reset means "register afresh", which the bridge does by building a
/// new provider (relaunch-equivalent; `CXProvider.invalidate` is a no-op on
/// iOS 26). A reset taken in the background is registered again on the next
/// foreground, unless the provider has answered in between. And a start
/// refused with `unknownCallProvider` rebuilds and retries once, so the
/// user's tap still places the call, ten seconds late instead of never.
public struct ProviderRecovery: Equatable, Sendable {
    public enum Action: Equatable, Sendable {
        /// Build a new `CXProvider` and hand it the delegate. The string is
        /// the reason, for the log.
        case rebuild(String)
        /// Request the start action for this call again, on the new provider.
        case retryStart(String)
        /// The call is refused for good; tell the controller.
        case giveUp(String)
    }

    /// `CXErrorCodeRequestTransactionError.unknownCallProvider` (CXError.h).
    public static let unknownCallProviderCode = 2

    /// A reset was taken while the app was not active and the provider has
    /// not been heard from since.
    public private(set) var resetTakenInBackground = false
    /// Calls whose start has already been retried once.
    public private(set) var retried: Set<String> = []

    public init() {}

    /// `providerDidReset`. `appActive` is `UIApplication.applicationState == .active`.
    public mutating func didReset(appActive: Bool) -> [Action] {
        resetTakenInBackground = !appActive
        return [.rebuild(appActive ? "provider reset" : "provider reset, taken in the background")]
    }

    /// `UIApplication.didBecomeActiveNotification`.
    public mutating func didBecomeActive() -> [Action] {
        guard resetTakenInBackground else { return [] }
        resetTakenInBackground = false
        return [.rebuild("app active after a reset taken in the background")]
    }

    /// The system answered a call on the current provider (a report was
    /// accepted, or a transaction was handed to the delegate): the
    /// registration is proven and nothing is owed on the next foreground.
    public mutating func providerAnswered() {
        resetTakenInBackground = false
    }

    /// A start request came back with an error. `code` is the raw
    /// `CXErrorCodeRequestTransactionError`, or anything else for another
    /// domain.
    public mutating func startFailed(callID: String, code: Int) -> [Action] {
        guard code == Self.unknownCallProviderCode else {
            retried.remove(callID)
            return [.giveUp(callID)]
        }
        if retried.contains(callID) {
            retried.remove(callID)
            return [.giveUp(callID)]
        }
        retried.insert(callID)
        return [.rebuild("start request refused: the system knows no call provider for this app"), .retryStart(callID)]
    }

    /// The start went through (the delegate performed it) or the call went
    /// away: forget its retry.
    public mutating func forget(_ callID: String) {
        retried.remove(callID)
    }
}

/// The calls the bridge has told CallKit about, with whether CallKit has
/// accepted each one yet, so "is another call up?" is answered the same way
/// for both directions.
///
/// A call is UP once CallKit has accepted it: an incoming call whose report
/// has completed without error, or an outgoing call whose start action the
/// delegate has performed. Before that it is a request that can still be
/// refused, and it must not count: an incoming report refused late by a
/// Focus filter (2026-09-17), or an outgoing start that sat in the system
/// for 10 s and was refused with `unknownCallProvider` (2026-09-27, out-16),
/// each made the app treat the NEXT incoming call as call waiting: no
/// audio-session setup for it, the call-waiting beep at nobody, and a
/// caller who gave up.
public struct CallRoster: Equatable, Sendable {
    public enum State: Equatable, Sendable {
        case awaitingReport
        case awaitingStart
        case up
    }

    private var states: [String: State] = [:]

    public init() {}

    public var ids: Set<String> { Set(states.keys) }
    public var isEmpty: Bool { states.isEmpty }
    public func contains(_ callID: String) -> Bool { states[callID] != nil }
    public func state(of callID: String) -> State? { states[callID] }

    /// CallKit has accepted at least one call.
    public var anyUp: Bool { states.values.contains(.up) }

    public mutating func add(_ callID: String, _ state: State) {
        states[callID] = state
    }

    /// The report completed without error, or the start was performed.
    public mutating func confirm(_ callID: String) {
        guard states[callID] != nil else { return }
        states[callID] = .up
    }

    public mutating func remove(_ callID: String) {
        states[callID] = nil
    }

    public mutating func removeAll() {
        states.removeAll()
    }
}
