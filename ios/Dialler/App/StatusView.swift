import DiallerCore
import SwiftUI

/// The hidden Status page (SPEC §6 item 8): pushed onto the Settings tab by
/// a five-second press on its title, popped by Back or any tab-bar tap.
/// An engineering surface, not a user one: raw states and the log.
struct StatusView: View {
    @EnvironmentObject private var model: AppModel
    /// Comma-separated, as typed; parsed by `SSIDList` on save.
    @State private var ssids = ""

    var body: some View {
        List {
            Section("Gateway") {
                LabeledContent("Status", value: model.status)
                if !model.sessionID.isEmpty { LabeledContent("Session", value: model.sessionID) }
                LabeledContent("Engine", value: model.engineState)
                HStack {
                    Button("Connect") { model.connect() }
                    Spacer()
                    Button("Disconnect", role: .destructive) { model.disconnect() }
                }
                .buttonStyle(.borderless)
            }
            // The dev path (SPEC §6 item 8): the fields that used to be
            // Settings, for a harness with fixed tokens and no camera.
            Section {
                TextField("Host", text: $model.host).textInputAutocapitalization(.never).autocorrectionDisabled()
                TextField("Port", text: $model.port).keyboardType(.numberPad)
                TextField("Device ID", text: $model.deviceID).textInputAutocapitalization(.never).autocorrectionDisabled()
                SecureField("Token", text: $model.token)
                Toggle("Accept Any Certificate", isOn: $model.acceptAnyCertificate)
                Button("Save and Connect") { model.connect() }
            } header: {
                Text("Connection (Development)")
            } footer: {
                if model.certSHA256 != nil {
                    Text("A certificate is pinned from enrolment, so Accept Any Certificate is ignored.")
                }
            }
            // Local Push by hand, for a server that has no settings for this
            // device (the harness) and for clearing a stale configuration.
            Section("Local Push (Development)") {
                TextField("Office Wi-Fi SSIDs, separated by commas", text: $ssids)
                    .textInputAutocapitalization(.never).autocorrectionDisabled()
                Button("Turn On Background Calls for These Networks") { model.configureLocalPush(ssids: SSIDList.parse(ssids)) }
                    .disabled(SSIDList.parse(ssids).isEmpty)
                Button("Remove Saved Configuration", role: .destructive) { model.removeLocalPush() }
                LabeledContent("State", value: model.localPushStatus)
                LabeledContent("Background Calls", value: model.backgroundCalls)
            }
            .onAppear { if ssids.isEmpty { ssids = SSIDList.format(model.localPushSSIDs) } }
            Section("Log") {
                Button("Send Diagnostics to the Server") { Task { await model.sendDiagnostics(reason: "manual") } }
                if !model.diagnosticsStatus.isEmpty {
                    Text(model.diagnosticsStatus).font(.caption).foregroundStyle(.secondary)
                }
                ForEach(Array(model.log.enumerated().reversed()), id: \.offset) { _, line in
                    Text(line).font(.caption.monospaced())
                }
            }
        }
        .navigationTitle("Status")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
    }
}
