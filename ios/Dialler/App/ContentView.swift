import DiallerCore
import SwiftUI

struct ContentView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        TabView {
            StatusView().tabItem { Label("Status", systemImage: "antenna.radiowaves.left.and.right") }
            DirectoryView().tabItem { Label("Directory", systemImage: "person.2") }
            SettingsView().tabItem { Label("Settings", systemImage: "gear") }
        }
        // Call UI is CallKit's alone: the system incoming-call screen or
        // banner, the in-call screen when answered from the lock screen, the
        // green indicator when the app is in front, and Recents.
    }
}

struct StatusView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        NavigationStack {
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
                }
                Section("Log") {
                    ForEach(Array(model.log.enumerated().reversed()), id: \.offset) { _, line in
                        Text(line).font(.caption.monospaced())
                    }
                }
            }
            .navigationTitle("Dialler")
        }
    }
}

struct DirectoryView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        NavigationStack {
            List(model.contacts) { c in
                VStack(alignment: .leading) {
                    Text(c.displayName)
                    Text(c.uri).font(.caption).foregroundStyle(.secondary)
                }
                .badge(c.mode)
            }
            .overlay {
                if model.contacts.isEmpty {
                    ContentUnavailableView("No contacts yet", systemImage: "person.2", description: Text("Connect, then refresh."))
                }
            }
            .navigationTitle("Directory")
            .toolbar {
                Button { Task { await model.syncDirectory() } } label: { Image(systemName: "arrow.clockwise") }
            }
        }
    }
}

struct SettingsView: View {
    @EnvironmentObject private var model: AppModel
    @State private var ssid = ""

    var body: some View {
        NavigationStack {
            Form {
                Section("Light server") {
                    TextField("Host", text: $model.host).textInputAutocapitalization(.never).autocorrectionDisabled()
                    TextField("Port", text: $model.port).keyboardType(.numberPad)
                    Toggle("Accept self-signed certificate (dev)", isOn: $model.acceptAnyCertificate)
                }
                Section("Device enrolment") {
                    TextField("Device ID", text: $model.deviceID).textInputAutocapitalization(.never).autocorrectionDisabled()
                    SecureField("Token", text: $model.token)
                    Text("Issue with: POST /v1/admin/devices on the server (see harness/provision.sh).")
                        .font(.caption).foregroundStyle(.secondary)
                }
                Section("Local Push Connectivity (device only)") {
                    TextField("Office Wi-Fi SSID", text: $ssid)
                    Button("Enable background wakeups on this SSID") { model.configureLocalPush(ssid: ssid) }
                        .disabled(ssid.isEmpty)
                    Button("Remove saved configuration", role: .destructive) { model.removeLocalPush() }
                    LabeledContent("State", value: model.localPushStatus)
                }
                Section {
                    Button("Save & connect") { model.connect() }
                }
            }
            .navigationTitle("Settings")
        }
    }
}
