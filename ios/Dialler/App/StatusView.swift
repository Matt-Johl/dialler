import DiallerCore
import SwiftUI

/// The hidden Status page (SPEC §6 item 8): pushed onto the Settings tab by
/// a five-second press on its title, popped by Back or any tab-bar tap.
/// An engineering surface: raw states, the manual connection, Local Push
/// by hand and the log — in the same look and wording as Settings. Release
/// builds show the states, diagnostics and the log only; the controls that
/// change the connection are for Debug builds (appstore.md, must-fix 4).
struct StatusView: View {
    @EnvironmentObject private var model: AppModel
    /// Comma-separated, as typed; parsed by `SSIDList` on save.
    @State private var ssids = ""

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                gateway
                #if DEBUG
                connection
                #endif
                backgroundCalls
                diagnostics
                log
            }
            .padding(.horizontal, 24)
            .padding(.bottom, 32)
        }
        .scrollDismissesKeyboard(.interactively)
        .background(Palette.ground)
        .navigationTitle("Status")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
        .onAppear { if ssids.isEmpty { ssids = SSIDList.format(model.localPushSSIDs) } }
    }

    // MARK: Sections

    private var gateway: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("Gateway")
            SettingsRow("Status", value: Self.sentence(model.status))
            if !model.sessionID.isEmpty { SettingsRow("Session", value: model.sessionID) }
            SettingsRow("SIP Engine", value: Self.sentence(model.engineState))
            #if DEBUG
            HStack(spacing: 10) {
                Button("Connect") { model.connect() }
                    .buttonStyle(WideButtonStyle(kind: .outline))
                Button("Disconnect") { model.disconnect() }
                    .buttonStyle(WideButtonStyle(kind: .destructive))
            }
            .padding(.top, 16)
            #endif
        }
    }

    /// The dev path (SPEC §6 item 8): the fields that used to be Settings,
    /// for a harness with fixed tokens and no camera.
    private var connection: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("Manual Connection")
            FieldRow("Server", text: $model.host, prompt: "Name or IP address")
            FieldRow("Port", text: $model.port, prompt: "7443", keyboard: .numberPad)
            FieldRow("Device ID", text: $model.deviceID, prompt: "Required")
            FieldRow("Token", text: $model.token, prompt: "Required", secure: true)
            Toggle(isOn: $model.acceptAnyCertificate) {
                Text("Accept Any Certificate").font(.callout).foregroundStyle(Palette.ink)
            }
            .frame(minHeight: 52)
            .overlay(alignment: .bottom) { Palette.hairline.frame(height: 1) }
            SettingsFooter(model.certSHA256 == nil
                ? "For a test server with a fixed token. Enrolment fills these in."
                : "For a test server with a fixed token. A certificate is pinned from enrolment, so Accept Any Certificate is ignored.")
            Button("Save and Connect") { model.connect() }
                .buttonStyle(WideButtonStyle(kind: .outline))
                .padding(.top, 16)
        }
    }

    /// Local Push by hand, for a server that has no settings for this device
    /// (the harness) and for clearing a stale configuration.
    private var backgroundCalls: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("Background Calls")
            SettingsRow("State", value: model.localPushStatus)
            SettingsRow("Provider", value: model.backgroundCalls)
            #if DEBUG
            FieldRow("Networks", text: $ssids, prompt: "Network names")
            SettingsFooter("The office Wi-Fi networks, separated by commas, for a server that doesn’t send them.")
            VStack(spacing: 10) {
                Button("Turn On for These Networks") { model.configureLocalPush(ssids: SSIDList.parse(ssids)) }
                    .buttonStyle(WideButtonStyle(kind: .outline))
                    .disabled(SSIDList.parse(ssids).isEmpty)
                Button("Remove Saved Configuration") { model.removeLocalPush() }
                    .buttonStyle(WideButtonStyle(kind: .destructive))
            }
            .padding(.top, 16)
            #endif
        }
    }

    private var diagnostics: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("Diagnostics")
            if !model.diagnosticsStatus.isEmpty {
                SettingsRow("Last Sent", value: model.diagnosticsStatus)
            }
            Button("Send Diagnostics to the Server") { Task { await model.sendDiagnostics(reason: "manual") } }
                .buttonStyle(WideButtonStyle(kind: .outline))
                .padding(.top, 16)
        }
    }

    /// Newest first, as written: the log is for reading against the
    /// server's, so its lines stay exactly as the app wrote them.
    private var log: some View {
        VStack(alignment: .leading, spacing: 0) {
            SettingsSection("Log")
            VStack(alignment: .leading, spacing: 0) {
                ForEach(Array(model.log.enumerated().reversed()), id: \.offset) { _, line in
                    Text(line)
                        .font(.caption.monospaced())
                        .foregroundStyle(Palette.secondary)
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.vertical, 8)
                        .overlay(alignment: .bottom) { Palette.hairline.frame(height: 1) }
                }
            }
            .textSelection(.enabled)
        }
    }

    /// A raw state with its first letter capitalised: "connecting to
    /// pbx:7443" reads "Connecting to pbx:7443".
    static func sentence(_ s: String) -> String {
        s.prefix(1).uppercased() + s.dropFirst()
    }
}

/// A labelled text field over a hairline: the label left, the value right.
private struct FieldRow: View {
    let label: String
    @Binding var text: String
    let prompt: String
    var keyboard: UIKeyboardType = .default
    var secure = false

    init(_ label: String, text: Binding<String>, prompt: String, keyboard: UIKeyboardType = .default, secure: Bool = false) {
        self.label = label
        self._text = text
        self.prompt = prompt
        self.keyboard = keyboard
        self.secure = secure
    }

    var body: some View {
        HStack(spacing: 16) {
            Text(label).font(.callout).foregroundStyle(Palette.ink)
            Spacer(minLength: 8)
            Group {
                if secure {
                    SecureField(label, text: $text, prompt: Text(prompt).foregroundStyle(Palette.tertiary))
                } else {
                    TextField(label, text: $text, prompt: Text(prompt).foregroundStyle(Palette.tertiary))
                }
            }
            .font(.subheadline)
            .foregroundStyle(Palette.ink)
            .multilineTextAlignment(.trailing)
            .keyboardType(keyboard)
            .textInputAutocapitalization(.never)
            .autocorrectionDisabled()
        }
        .padding(.vertical, 15)
        .frame(minHeight: 52)
        .overlay(alignment: .bottom) { Palette.hairline.frame(height: 1) }
    }
}
