import DiallerCore
import SwiftUI

struct InCallView: View {
    @EnvironmentObject private var model: AppModel
    @State private var transferTo = ""
    @State private var showTransfer = false

    var body: some View {
        ZStack {
            Palette.ground.ignoresSafeArea()
            // `activeCall` is nil only while the cover animates away after
            // the last call ended; the screen keeps its frame and shows
            // nothing.
            if let call = model.activeCall { content(for: call) }
        }
        .tint(Palette.ink)
        .interactiveDismissDisabled()
    }

    @ViewBuilder
    private func content(for call: AppModel.ActiveCall) -> some View {
        VStack(spacing: 0) {
            Spacer(minLength: 16)
            Rings(size: 196, inner: 0.52) {
                if let initials = CallText.initials(call.title) {
                    Text(initials)
                        .font(.system(size: 32, weight: .light))
                        .tracking(1)
                        .foregroundStyle(Palette.ink)
                } else {
                    Image(systemName: "phone").font(.system(size: 30, weight: .light)).foregroundStyle(Palette.ink)
                }
            }
            Text(call.title)
                .font(.system(size: 34, weight: .regular))
                .tracking(-1.2)
                .foregroundStyle(Palette.ink)
                .multilineTextAlignment(.center)
                .lineLimit(2)
                .minimumScaleFactor(0.6)
                .padding(.top, 28)
            statusLine(for: call)
                .padding(.top, 6)
            VStack(spacing: 8) {
                if let notice = model.notice {
                    Label(notice, systemImage: "exclamationmark.circle")
                        .transition(.opacity)
                }
                if let held = model.heldCall {
                    // Call waiting: the other call is on hold. Switching
                    // between the two is the system's job — iOS 26 shows its
                    // own Swap banner over the app while a call is held — so
                    // this screen only names the held party. Ending the held
                    // call is swap, then End, as in the Phone app.
                    Label("\(held.title) · On Hold", systemImage: "pause.circle")
                }
            }
            .font(.footnote)
            .foregroundStyle(Palette.secondary)
            .lineLimit(2)
            .multilineTextAlignment(.center)
            .padding(.top, 16)
            Spacer(minLength: 24)
            controls(for: call)
            Button(action: { model.hangUp() }) {
                Image(systemName: "phone.down.fill").font(.system(size: 28))
            }
            .buttonStyle(CircleButtonStyle(size: 76, fill: Palette.end))
            .accessibilityLabel("End Call")
            .padding(.top, 40)
            .padding(.bottom, 28)
        }
        .padding(.horizontal, 24)
        .alert("Transfer Call", isPresented: $showTransfer) {
            TextField("Number or extension", text: $transferTo)
                .keyboardType(.numbersAndPunctuation)
                .textInputAutocapitalization(.never)
            Button("Transfer") { model.transfer(to: transferTo); transferTo = "" }
            Button("Cancel", role: .cancel) { transferTo = "" }
        } message: {
            Text("The caller is connected to this number and your call ends.")
        }
    }

    @ViewBuilder
    private func statusLine(for call: AppModel.ActiveCall) -> some View {
        Group {
            if let started = call.connectedAt, !call.held {
                TimelineView(.periodic(from: started, by: 1)) { ctx in
                    Text(Self.duration(from: started, to: ctx.date)).monospacedDigit()
                }
            } else {
                Text(call.status)
            }
        }
        .font(.title3.weight(.light))
        .foregroundStyle(Palette.secondary)
    }

    private func controls(for call: AppModel.ActiveCall) -> some View {
        HStack(alignment: .top, spacing: 0) {
            CallControl(title: "Mute", glyph: "mic.slash", on: call.muted) { model.toggleMute() }
            if model.heldCall == nil {
                CallControl(title: "Hold", glyph: "pause", on: call.held) { model.toggleHold() }
            } else if !Self.systemSwapsCalls {
                // iOS 17/18 show no swap banner over a foreground app:
                // the Hold button becomes Swap, as on the Phone app.
                CallControl(title: "Swap", glyph: "arrow.left.arrow.right", on: false) { model.swapCalls() }
            }
            CallControl(title: "Speaker", glyph: "speaker.wave.2", on: call.speaker) { model.toggleSpeaker() }
            CallControl(title: "Transfer", glyph: "arrow.turn.up.right", on: false) { showTransfer = true }
                .disabled(call.connectedAt == nil)
        }
    }

    /// iOS 26 swaps a held and an active call from its own banner, shown over
    /// the app whenever one of its calls is held (device, 2026-09-14).
    static var systemSwapsCalls: Bool {
        if #available(iOS 26, *) { return true } else { return false }
    }

    static func duration(from start: Date, to now: Date) -> String {
        let s = max(0, Int(now.timeIntervalSince(start)))
        return s >= 3600 ? String(format: "%d:%02d:%02d", s / 3600, s / 60 % 60, s % 60) : String(format: "%d:%02d", s / 60, s % 60)
    }
}

/// One in-call control: an outlined circle that fills with ink while on.
/// The label stays the same either way, as on the Phone app; VoiceOver
/// hears the state.
private struct CallControl: View {
    let title: String
    let glyph: String
    let on: Bool
    let action: () -> Void

    var body: some View {
        VStack(spacing: 8) {
            Button(action: action) {
                Image(systemName: glyph).font(.system(size: 22, weight: .regular))
            }
            .buttonStyle(CircleButtonStyle(size: 68, on: on))
            Text(title)
                .font(.caption)
                .foregroundStyle(Palette.secondary)
        }
        .frame(maxWidth: .infinity)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(title)
        .accessibilityAddTraits(.isButton)
        .accessibilityValue(on ? "On" : "Off")
    }
}
