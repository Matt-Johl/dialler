import DiallerCore
import SwiftUI

struct ContentView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        TabView {
            KeypadView().tabItem { Label("Keypad", systemImage: "circle.grid.3x3") }
            DirectoryView().tabItem { Label("Directory", systemImage: "person.2") }
            StatusView().tabItem { Label("Status", systemImage: "antenna.radiowaves.left.and.right") }
            SettingsView().tabItem { Label("Settings", systemImage: "gear") }
        }
        // Ringing is CallKit's UI alone (banner, lock screen, Recents). While
        // a call is ACTIVE and the app is in front, iOS shows only the green
        // status indicator, so the in-call controls are ours.
        .fullScreenCover(item: Binding(get: { model.activeCall }, set: { _ in })) { call in
            InCallView(call: call)
        }
    }
}

// MARK: - In-call

struct InCallView: View {
    @EnvironmentObject private var model: AppModel
    let call: AppModel.ActiveCall
    @State private var transferTo = ""
    @State private var showTransfer = false

    var body: some View {
        VStack(spacing: 32) {
            Spacer()
            Text(call.title).font(.largeTitle.weight(.semibold)).multilineTextAlignment(.center)
            if let started = call.connectedAt {
                TimelineView(.periodic(from: started, by: 1)) { ctx in
                    Text(Self.duration(from: started, to: ctx.date)).font(.title2.monospacedDigit()).foregroundStyle(.secondary)
                }
            } else {
                Text(call.status).font(.title2).foregroundStyle(.secondary)
            }
            Spacer()
            HStack(spacing: 28) {
                CallButton(title: call.muted ? "Unmute" : "Mute", system: call.muted ? "mic.slash.fill" : "mic.fill", active: call.muted) { model.toggleMute() }
                CallButton(title: call.held ? "Resume" : "Hold", system: call.held ? "play.fill" : "pause.fill", active: call.held) { model.toggleHold() }
                CallButton(title: "Speaker", system: "speaker.wave.2.fill", active: call.speaker) { model.toggleSpeaker() }
                CallButton(title: "Transfer", system: "arrow.turn.up.right", active: false) { showTransfer = true }
                    .disabled(call.connectedAt == nil)
            }
            .alert("Transfer call to", isPresented: $showTransfer) {
                TextField("Number or user", text: $transferTo)
                    .keyboardType(.numbersAndPunctuation)
                    .textInputAutocapitalization(.never)
                Button("Transfer") { model.transfer(to: transferTo); transferTo = "" }
                Button("Cancel", role: .cancel) { transferTo = "" }
            } message: {
                Text("The other party is connected to this number and this call ends.")
            }
            Button(action: { model.hangUp() }) {
                Image(systemName: "phone.down.fill")
                    .font(.title)
                    .foregroundStyle(.white)
                    .frame(width: 76, height: 76)
                    .background(Color.red, in: Circle())
            }
            .padding(.bottom, 40)
        }
        .padding()
        .interactiveDismissDisabled()
    }

    static func duration(from start: Date, to now: Date) -> String {
        let s = max(0, Int(now.timeIntervalSince(start)))
        return s >= 3600 ? String(format: "%d:%02d:%02d", s / 3600, s / 60 % 60, s % 60) : String(format: "%d:%02d", s / 60, s % 60)
    }
}

private struct CallButton: View {
    let title: String
    let system: String
    let active: Bool
    let action: () -> Void

    var body: some View {
        VStack(spacing: 8) {
            Button(action: action) {
                Image(systemName: system)
                    .font(.title2)
                    .foregroundStyle(active ? Color.black : Color.primary)
                    .frame(width: 64, height: 64)
                    .background(active ? Color.white : Color.secondary.opacity(0.25), in: Circle())
            }
            Text(title).font(.caption).foregroundStyle(.secondary)
        }
    }
}

// MARK: - Keypad

struct KeypadView: View {
    @EnvironmentObject private var model: AppModel
    @State private var number = ""

    private let keys: [[String]] = [["1", "2", "3"], ["4", "5", "6"], ["7", "8", "9"], ["*", "0", "#"]]

    var body: some View {
        NavigationStack {
            VStack(spacing: 24) {
                Spacer()
                Text(number.isEmpty ? " " : number)
                    .font(.system(size: 36, weight: .light, design: .rounded).monospacedDigit())
                    .lineLimit(1)
                    .minimumScaleFactor(0.5)
                    .frame(maxWidth: .infinity)
                    .padding(.horizontal)
                VStack(spacing: 14) {
                    ForEach(keys, id: \.self) { row in
                        HStack(spacing: 24) {
                            ForEach(row, id: \.self) { key in
                                Button(key) { number.append(key) }
                                    .font(.system(size: 30, weight: .regular, design: .rounded))
                                    .frame(width: 76, height: 76)
                                    .background(Color.secondary.opacity(0.18), in: Circle())
                                    .foregroundStyle(.primary)
                            }
                        }
                    }
                }
                HStack(spacing: 24) {
                    Color.clear.frame(width: 76, height: 76)
                    Button(action: { model.dial(number) }) {
                        Image(systemName: "phone.fill")
                            .font(.title)
                            .foregroundStyle(.white)
                            .frame(width: 76, height: 76)
                            .background(number.isEmpty ? Color.gray : Color.green, in: Circle())
                    }
                    .disabled(number.isEmpty || model.activeCall != nil)
                    Button(action: { if !number.isEmpty { number.removeLast() } }) {
                        Image(systemName: "delete.left")
                            .font(.title2)
                            .frame(width: 76, height: 76)
                    }
                    .foregroundStyle(number.isEmpty ? .clear : .secondary)
                    .simultaneousGesture(LongPressGesture().onEnded { _ in number = "" })
                }
                Spacer()
            }
            .navigationTitle("Keypad")
        }
    }
}

// MARK: - Directory

struct DirectoryView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        NavigationStack {
            List(model.contacts) { c in
                Button {
                    model.dial(c.uri)
                } label: {
                    HStack {
                        VStack(alignment: .leading) {
                            Text(c.displayName).foregroundStyle(.primary)
                            Text(c.uri).font(.caption).foregroundStyle(.secondary)
                        }
                        Spacer()
                        Image(systemName: "phone.fill").foregroundStyle(.green)
                    }
                }
                .badge(c.mode)
                .disabled(model.activeCall != nil)
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

// MARK: - Status

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

// MARK: - Settings

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
