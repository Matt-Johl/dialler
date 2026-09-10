import AVFoundation
import CallKit
import DiallerCore
import Foundation
import UIKit
import os

/// CallKit as a `CallUI` (SPEC §5 component 7): reports incoming calls and
/// forwards the user's answer/end actions to the controller.
///
/// Every CallKit callback is echoed to the app log with a "callkit:" prefix,
/// including the transaction receipt (`provider(_:execute:)`), the system's
/// own view of the call (`CXCallObserver`), resets, action time-outs, and
/// audio-session interruptions — so a tap that "does nothing" shows exactly
/// how far it got.
final class CallKitBridge: NSObject, CallUI, CXProviderDelegate, CXCallObserverDelegate {
    var onAnswer: (String) -> Void = { _ in }
    var onEnd: (String) -> Void = { _ in }
    /// CallKit approved an outgoing call we asked for: dial now.
    var onStart: (String) -> Void = { _ in }
    /// CallKit refused the outgoing call.
    var onStartFailed: (String) -> Void = { _ in }
    /// Our outgoing call was answered (reported to CallKit as connected).
    var onConnected: (String) -> Void = { _ in }
    var onMute: (String, Bool) -> Void = { _, _ in }
    var onHold: (String, Bool) -> Void = { _, _ in }
    var onAudioActivated: () -> Void = {}
    var onAudioDeactivated: () -> Void = {}
    /// Bridge events for the app's visible log (os_log alone was invisible
    /// while diagnosing on a device).
    var onLog: (String) -> Void = { _ in }

    private let provider: CXProvider
    private let controller = CXCallController()
    private let observer = CXCallObserver()
    private var uuids: [String: UUID] = [:]
    private var callIDs: [UUID: String] = [:]
    private var lastUpdate: [UUID: CXCallUpdate] = [:]
    private var notificationTokens: [NSObjectProtocol] = []

    override init() {
        let config = CXProviderConfiguration()
        config.supportsVideo = false
        config.maximumCallGroups = 1
        config.maximumCallsPerCallGroup = 1
        config.supportedHandleTypes = [.generic]
        config.includesCallsInRecents = true
        provider = CXProvider(configuration: config)
        super.init()
        provider.setDelegate(self, queue: .main)
        observer.setDelegate(self, queue: .main)
        observeSystemNotifications()
        // Category, mode and hardware preferences are set once, at launch.
        // If the first call is also the first time they are applied, the
        // change only takes effect when CallKit activates the session, the
        // hardware is still reconfiguring while the engine creates its
        // VoiceProcessingIO units, and every render fails for that call
        // (observed: route change reason 3 at activation, then render -1).
        configureAudioSession()
        requestMicrophonePermission()
    }

    /// The recorder unit needs record permission; without it the voice
    /// processing unit fails to render. Ask at launch, not mid-call.
    private func requestMicrophonePermission() {
        switch AVAudioApplication.shared.recordPermission {
        case .granted:
            onLog("audio: microphone permission granted")
        case .denied:
            onLog("audio: microphone permission DENIED — calls will be silent; enable it in Settings")
        case .undetermined:
            AVAudioApplication.requestRecordPermission { [weak self] granted in
                DispatchQueue.main.async { self?.onLog("audio: microphone permission \(granted ? "granted" : "DENIED")") }
            }
        @unknown default:
            break
        }
    }

    deinit {
        notificationTokens.forEach { NotificationCenter.default.removeObserver($0) }
    }

    // MARK: In-app controls → CallKit
    // Requests go through CXCallController and land in the same provider
    // delegate as the system UI's own actions.

    func requestAnswer(callID: String) {
        guard let uuid = uuids[callID] else { return }
        controller.request(CXTransaction(action: CXAnswerCallAction(call: uuid))) { [weak self] err in
            if let err { self?.onLog("callkit: answer request failed: \(err.localizedDescription)") }
        }
    }

    func requestEnd(callID: String) {
        guard let uuid = uuids[callID] else { return }
        controller.request(CXTransaction(action: CXEndCallAction(call: uuid))) { [weak self] err in
            if let err { self?.onLog("callkit: end request failed: \(err.localizedDescription)") }
        }
    }

    func requestHold(callID: String, held: Bool) {
        guard let uuid = uuids[callID] else { return }
        controller.request(CXTransaction(action: CXSetHeldCallAction(call: uuid, onHold: held))) { [weak self] err in
            if let err { self?.onLog("callkit: hold request failed: \(err.localizedDescription)") }
        }
    }

    func requestMute(callID: String, muted: Bool) {
        guard let uuid = uuids[callID] else { return }
        controller.request(CXTransaction(action: CXSetMutedCallAction(call: uuid, muted: muted))) { [weak self] err in
            if let err { self?.onLog("callkit: mute request failed: \(err.localizedDescription)") }
        }
    }

    // MARK: CallUI — outgoing

    /// Outgoing calls go through CallKit too: request a start action, and
    /// dial only when CallKit performs it (so the system call bar, Recents
    /// and audio-session activation all work as for incoming calls).
    func startOutgoing(callID: String, handle: String, displayName: String) {
        DispatchQueue.main.async { [self] in
            let uuid = UUID()
            uuids[callID] = uuid
            callIDs[uuid] = callID
            let action = CXStartCallAction(call: uuid, handle: CXHandle(type: .generic, value: handle))
            action.contactIdentifier = displayName
            onLog("callkit: requesting outgoing call \(callID) to \(handle) as \(short(uuid))")
            controller.request(CXTransaction(action: action)) { [weak self] err in
                guard let self, let err else { return }
                self.onLog("callkit: start request failed: \(err.localizedDescription)")
                self.uuids[callID] = nil
                self.callIDs[uuid] = nil
                self.onStartFailed(callID)
            }
        }
    }

    func outgoingConnecting(callID: String) {
        DispatchQueue.main.async { [self] in
            guard let uuid = uuids[callID] else { return }
            provider.reportOutgoingCall(with: uuid, startedConnectingAt: Date())
            onLog("callkit: \(callID) connecting")
        }
    }

    func outgoingConnected(callID: String) {
        DispatchQueue.main.async { [self] in
            guard let uuid = uuids[callID] else { return }
            provider.reportOutgoingCall(with: uuid, connectedAt: Date())
            onLog("callkit: \(callID) connected")
            onConnected(callID)
        }
    }

    // MARK: CallUI

    // The controller calls these from whichever thread delivered the event
    // (gateway socket, PushKit, libre loop); CallKit wants the main thread.

    func reportIncoming(callID: String, displayName: String, handle: String, completion: @escaping (Error?) -> Void) {
        DispatchQueue.main.async { [self] in
            configureAudioSession()
            let uuid = UUID()
            uuids[callID] = uuid
            callIDs[uuid] = callID
            let update = CXCallUpdate()
            update.remoteHandle = CXHandle(type: .generic, value: handle)
            update.localizedCallerName = displayName
            update.hasVideo = false
            lastUpdate[uuid] = update
            onLog("callkit: reporting \(callID) as \(short(uuid)) (app \(appState))")
            provider.reportNewIncomingCall(with: uuid, update: update) { [weak self] error in
                guard let self else { return }
                if let error {
                    self.onLog("callkit: report of \(callID) refused: \(error.localizedDescription)")
                    self.uuids[callID] = nil
                    self.callIDs[uuid] = nil
                    self.lastUpdate[uuid] = nil
                } else {
                    self.onLog("callkit: report of \(callID) accepted")
                }
                completion(error)
            }
        }
    }

    /// A better caller name learned while the call is still ringing (the
    /// wake arrived after the INVITE had rung it with only the peer URI).
    func updateIncoming(callID: String, displayName: String) {
        DispatchQueue.main.async { [self] in
            guard let uuid = uuids[callID] else {
                onLog("callkit: name update for \(callID): no CallKit call")
                return
            }
            let update = lastUpdate[uuid] ?? CXCallUpdate()
            update.localizedCallerName = displayName
            lastUpdate[uuid] = update
            provider.reportCall(with: uuid, updated: update)
            onLog("callkit: \(callID) caller name updated to \(displayName)")
        }
    }

    /// A PushKit delivery for a call that is already ringing. iOS requires
    /// every VoIP push to be answered with `reportNewIncomingCall`; doing so
    /// again with the same UUID is refused (already exists) but satisfies
    /// that requirement, and leaves the ringing call untouched.
    func reaffirm(callID: String) {
        DispatchQueue.main.async { [self] in
            guard let uuid = uuids[callID] else {
                onLog("callkit: reaffirm of \(callID): no CallKit call to reaffirm")
                return
            }
            let update = lastUpdate[uuid] ?? CXCallUpdate()
            provider.reportNewIncomingCall(with: uuid, update: update) { [weak self] error in
                self?.onLog("callkit: reaffirmed \(callID) (\(error.map { $0.localizedDescription } ?? "accepted"))")
            }
        }
    }

    func end(callID: String, reason: CallEndReason) {
        DispatchQueue.main.async { [self] in
            guard let uuid = uuids.removeValue(forKey: callID) else { return }
            callIDs[uuid] = nil
            lastUpdate[uuid] = nil
            let cxReason: CXCallEndedReason
            switch reason {
            case .remoteEnded: cxReason = .remoteEnded
            case .answeredElsewhere: cxReason = .answeredElsewhere
            case .unanswered: cxReason = .unanswered
            case .failed: cxReason = .failed
            }
            onLog("callkit: ending \(callID) (\(reason))")
            provider.reportCall(with: uuid, endedAt: Date(), reason: cxReason)
            onEnded(callID)
        }
    }

    /// A call was ended by the far end or failed (not by the user).
    var onEnded: (String) -> Void = { _ in }

    /// Set the category and mode for the call; never activate the session —
    /// CallKit activates it and calls didActivate.
    private func configureAudioSession() {
        let session = AVAudioSession.sharedInstance()
        do {
            try session.setCategory(.playAndRecord, mode: .voiceChat, options: [.allowBluetooth])
            // What baresip's audiounit module (VoiceProcessingIO) is configured
            // for: 48 kHz, 20 ms frames. Preferences only; iOS may adjust.
            try session.setPreferredSampleRate(48000)
            try session.setPreferredIOBufferDuration(0.02)
        } catch {
            logger.error("audio session configure: \(error.localizedDescription, privacy: .public)")
            onLog("callkit: audio session configure failed: \(error.localizedDescription)")
        }
    }

    private let logger = Logger(subsystem: DiallerIDs.bundlePrefix, category: "callkit")

    private func short(_ uuid: UUID) -> String { String(uuid.uuidString.prefix(8)) }

    private var appState: String {
        switch UIApplication.shared.applicationState {
        case .active: return "active"
        case .inactive: return "inactive"
        case .background: return "background"
        @unknown default: return "?"
        }
    }

    /// Audio-session interruptions and app state changes, logged around the
    /// answer so a silent tap can be placed against what the system did.
    private func observeSystemNotifications() {
        let nc = NotificationCenter.default
        notificationTokens.append(nc.addObserver(forName: AVAudioSession.interruptionNotification, object: nil, queue: .main) { [weak self] n in
            let raw = n.userInfo?[AVAudioSessionInterruptionTypeKey] as? UInt ?? 99
            let reason = n.userInfo?[AVAudioSessionInterruptionReasonKey] as? UInt
            self?.onLog("callkit: audio interruption type=\(raw == 1 ? "began" : raw == 0 ? "ended" : "\(raw)") reason=\(reason.map(String.init) ?? "-")")
        })
        notificationTokens.append(nc.addObserver(forName: AVAudioSession.routeChangeNotification, object: nil, queue: .main) { [weak self] n in
            let reason = n.userInfo?[AVAudioSessionRouteChangeReasonKey] as? UInt ?? 99
            let route = AVAudioSession.sharedInstance().currentRoute.outputs.first?.portName ?? "?"
            self?.onLog("callkit: audio route change reason=\(reason) now \(route)")
        })
        notificationTokens.append(nc.addObserver(forName: UIApplication.willResignActiveNotification, object: nil, queue: .main) { [weak self] _ in
            self?.onLog("app: will resign active")
        })
        notificationTokens.append(nc.addObserver(forName: UIApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in
            self?.onLog("app: did become active")
        })
    }

    // MARK: CXCallObserverDelegate — the system's view of each call

    func callObserver(_: CXCallObserver, callChanged call: CXCall) {
        let id = callIDs[call.uuid] ?? "unknown"
        onLog("callkit: observer \(short(call.uuid)) (\(id)) outgoing=\(call.isOutgoing) connected=\(call.hasConnected) ended=\(call.hasEnded) held=\(call.isOnHold)")
    }

    // MARK: CXProviderDelegate

    /// Called before the individual actions of every transaction CallKit
    /// hands us. Returning false lets the per-action methods run.
    func provider(_: CXProvider, execute transaction: CXTransaction) -> Bool {
        let names = transaction.actions.map { action -> String in
            let name = String(describing: type(of: action)).replacingOccurrences(of: "CX", with: "")
            let call = (action as? CXCallAction).map { short($0.callUUID) } ?? "-"
            return "\(name)(\(call))"
        }
        onLog("callkit: transaction \(names.joined(separator: ",")) (app \(appState))")
        return false
    }

    func providerDidBegin(_: CXProvider) {
        onLog("callkit: provider began")
    }

    func provider(_: CXProvider, timedOutPerforming action: CXAction) {
        onLog("callkit: TIMED OUT performing \(String(describing: type(of: action)))")
    }

    func provider(_: CXProvider, didActivate session: AVAudioSession) {
        let route = session.currentRoute.outputs.first?.portName ?? "?"
        let input = session.currentRoute.inputs.first?.portName ?? "none"
        logger.info("audio session activated (\(route, privacy: .public))")
        onLog("callkit: audio session activated (out \(route), in \(input), \(Int(session.sampleRate))Hz, io \(Int(session.ioBufferDuration * 1000))ms, inputAvailable=\(session.isInputAvailable), inCh=\(session.inputNumberOfChannels), outCh=\(session.outputNumberOfChannels), otherAudio=\(session.isOtherAudioPlaying))")
        // The call was answered in the action; the engine binds its audio
        // devices now (or on establishment, whichever comes last).
        onAudioActivated()
    }

    func provider(_: CXProvider, didDeactivate _: AVAudioSession) {
        logger.info("audio session deactivated")
        onLog("callkit: audio session deactivated")
        onAudioDeactivated()
    }

    func providerDidReset(_: CXProvider) {
        onLog("callkit: PROVIDER RESET (\(uuids.count) call(s) dropped)")
        for id in uuids.keys { onEnd(id) }
        uuids.removeAll()
        callIDs.removeAll()
        lastUpdate.removeAll()
    }

    func provider(_: CXProvider, perform action: CXAnswerCallAction) {
        guard let id = callIDs[action.callUUID] else {
            onLog("callkit: answer for unknown call \(short(action.callUUID)); known: \(callIDs.keys.map(short))")
            action.fail()
            return
        }
        let session = AVAudioSession.sharedInstance()
        onLog("audio: at answer category=\(session.category.rawValue) mode=\(session.mode.rawValue) mic=\(AVAudioApplication.shared.recordPermission.rawValue) rate=\(Int(session.sampleRate))")
        // Answer in the action and fulfill (Speakerbox order). CallKit then
        // activates the session and calls didActivate; the engine binds its
        // audio devices once both the call and the session are up.
        onLog("callkit: answer accepted for \(id)")
        onAnswer(id)
        action.fulfill()
    }

    func provider(_: CXProvider, perform action: CXEndCallAction) {
        if let id = callIDs.removeValue(forKey: action.callUUID) {
            uuids[id] = nil
            lastUpdate[action.callUUID] = nil
            onLog("callkit: end for \(id)")
            onEnd(id)
        } else {
            onLog("callkit: end for unknown call \(short(action.callUUID))")
        }
        action.fulfill()
    }

    func provider(_: CXProvider, perform action: CXStartCallAction) {
        guard let id = callIDs[action.callUUID] else {
            onLog("callkit: start for unknown call \(short(action.callUUID))")
            action.fail()
            return
        }
        onLog("callkit: start accepted for \(id) → \(action.handle.value)")
        // Same shape as answering: hand it to the engine, fulfill; CallKit
        // then activates the session and the engine releases its audio.
        onStart(id)
        action.fulfill()
    }

    func provider(_: CXProvider, perform action: CXSetMutedCallAction) {
        if let id = callIDs[action.callUUID] {
            onLog("callkit: mute=\(action.isMuted) for \(id)")
            onMute(id, action.isMuted)
        }
        action.fulfill()
    }

    func provider(_: CXProvider, perform action: CXSetHeldCallAction) {
        if let id = callIDs[action.callUUID] {
            onLog("callkit: hold=\(action.isOnHold) for \(id)")
            onHold(id, action.isOnHold)
        }
        action.fulfill()
    }
}
