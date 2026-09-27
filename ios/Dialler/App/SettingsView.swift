import DiallerCore
import SwiftUI
import UIKit

struct SettingsView: View {
    @EnvironmentObject private var model: AppModel
    /// Owned by `ContentView`, so a tab-bar tap can pop the hidden page.
    @Binding var path: NavigationPath
    @State private var confirmSignOut = false

    enum Route: Hashable { case status }

    static var version: String {
        let info = Bundle.main.infoDictionary
        let v = info?["CFBundleShortVersionString"] as? String ?? "?"
        let b = info?["CFBundleVersion"] as? String ?? "?"
        return "\(v) (\(b))"
    }

    private var device: String { UIDevice.current.model }

    var body: some View {
        NavigationStack(path: $path) {
            VStack(spacing: 0) {
                // The title takes the gesture: a five-second press opens the
                // hidden Status page. Five seconds, on purpose: nothing a
                // user does by accident.
                ScreenHeader(title: "Settings")
                    .contentShape(Rectangle())
                    .onLongPressGesture(minimumDuration: 5) { path.append(Route.status) }
                ScrollView {
                    VStack(alignment: .leading, spacing: 0) {
                        summary
                        connection
                        calls
                        about
                        signOut
                    }
                    .padding(.horizontal, 24)
                    .padding(.bottom, 32)
                }
            }
            .background(Palette.ground)
            .toolbar(.hidden, for: .navigationBar)
            .navigationDestination(for: Route.self) { route in
                switch route {
                case .status: StatusView()
                }
            }
            .confirmationDialog("Sign out of \(AppName.display)?", isPresented: $confirmSignOut, titleVisibility: .visible) {
                Button("Sign Out", role: .destructive) { model.logout() }
            } message: {
                Text("Calls won’t reach this \(device) until it’s set up again with a new enrolment code from your administrator.")
            }
        }
    }

    // MARK: Sections

    /// The line and whether it is up: the design canvas's Status page,
    /// folded into the top of Settings (2026-09-27).
    private var summary: some View {
        HStack(spacing: 16) {
            ZStack {
                Circle().strokeBorder(Palette.ink, lineWidth: 1.25)
                Image(systemName: linkGlyph)
                    .font(.system(size: 20, weight: .regular))
                    .foregroundStyle(Palette.ink)
                    .contentTransition(.symbolEffect(.replace))
            }
            .frame(width: 56, height: 56)
            VStack(alignment: .leading, spacing: 3) {
                Text(model.line.isEmpty ? "This \(device)" : CallText.party(model.line))
                    .font(.title3.weight(.medium))
                    .tracking(-0.4)
                    .foregroundStyle(Palette.ink)
                Text(linkText)
                    .font(.subheadline)
                    .foregroundStyle(Palette.secondary)
            }
        }
        .padding(.top, 14)
        .padding(.bottom, 8)
        .accessibilityElement(children: .combine)
    }

    private var connection: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("Connection")
            SettingsRow("Server", value: model.host.isEmpty ? "—" : model.host)
            SettingsRow("Background Calls", value: backgroundValue)
            SettingsRow("Office Wi-Fi", value: officeNetworks)
            SettingsFooter(backgroundFooter)
        }
    }

    private var calls: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("Calls")
            Toggle(isOn: $model.callWaiting) {
                Text("Call Waiting").font(.callout).foregroundStyle(Palette.ink)
            }
            .tint(Palette.switchOn)
            .frame(minHeight: 52)
            .overlay(alignment: .bottom) { Palette.hairline.frame(height: 1) }
            SettingsFooter("When this is on, a second caller rings while you’re on a call. When it’s off, they hear a busy tone.")
        }
    }

    private var about: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("About")
            SettingsRow("Version", value: Self.version)
        }
    }

    private var signOut: some View {
        Button("Sign Out") { confirmSignOut = true }
            .buttonStyle(WideButtonStyle(kind: .outline))
            .padding(.top, 32)
    }

    // MARK: Words

    private var linkGlyph: String {
        switch model.link {
        case .connected: return "checkmark"
        case .connecting, .waiting: return "ellipsis"
        case .offline, .refused: return "exclamationmark"
        }
    }

    private var linkText: String {
        switch model.link {
        case .connected: return "Connected"
        case .connecting: return "Connecting…"
        case .waiting: return "Waiting for Network"
        case .offline: return "Not Connected"
        case .refused: return "Not Accepted by the Server"
        }
    }

    private var backgroundValue: String {
        switch model.backgroundCallsMode {
        case .on: return "On"
        case .waitingForNetwork: return "Waiting for Wi-Fi"
        case .off: return "Off"
        }
    }

    /// The networks the administrator set (SPEC §6 item 8b), or, until
    /// they have, whatever is saved on the phone.
    private var officeNetworks: String {
        let list = model.serverSSIDs ?? model.localPushSSIDs
        return list.isEmpty ? "None" : SSIDList.format(list)
    }

    private var backgroundFooter: String {
        let app = AppName.display
        switch model.backgroundCallsMode {
        case .on:
            return "Calls reach this \(device) on office Wi-Fi, even when \(app) is closed."
        case .waitingForNetwork:
            return "Calls reach this \(device) while \(app) is open. When it’s closed, they reach it only on office Wi-Fi."
        case .off:
            return model.serverSSIDs == nil
                ? "Calls reach this \(device) only while \(app) is open. Your administrator hasn’t set up office Wi-Fi for it yet."
                : "Calls reach this \(device) only while \(app) is open."
        }
    }
}

// MARK: - Rows

/// A section's small-capitals label, spaced like the canvas.
struct SettingsSection: View {
    let title: String
    init(_ title: String) { self.title = title }

    var body: some View {
        SectionLabel(text: title)
            .padding(.top, 28)
            .padding(.bottom, 4)
    }
}

/// A label and its value, over a hairline.
struct SettingsRow: View {
    let label: String
    let value: String
    init(_ label: String, value: String) {
        self.label = label
        self.value = value
    }

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 16) {
            Text(label).font(.callout).foregroundStyle(Palette.ink)
            Spacer(minLength: 8)
            Text(value)
                .font(.subheadline)
                .foregroundStyle(Palette.secondary)
                .multilineTextAlignment(.trailing)
                .lineLimit(2)
        }
        .padding(.vertical, 15)
        .frame(minHeight: 52)
        .overlay(alignment: .bottom) { Palette.hairline.frame(height: 1) }
        .accessibilityElement(children: .combine)
    }
}

/// Explanatory text under a section.
struct SettingsFooter: View {
    let text: String
    init(_ text: String) { self.text = text }

    var body: some View {
        Text(text)
            .font(.footnote)
            .foregroundStyle(Palette.secondary)
            .fixedSize(horizontal: false, vertical: true)
            .padding(.top, 10)
    }
}
