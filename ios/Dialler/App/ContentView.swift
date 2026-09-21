import DiallerCore
import SwiftUI

struct ContentView: View {
    @EnvironmentObject private var model: AppModel

    var body: some View {
        // Tab order per SPEC §6 item 6. Status stays, last, until item 8
        // moves its contents behind the hidden gesture.
        TabView {
            RecentsView()
                .tabItem { Label("Recents", systemImage: "clock") }
                .badge(model.unseenMissed)
            DirectoryView().tabItem { Label("Directory", systemImage: "person.2") }
            KeypadView().tabItem { Label("Keypad", systemImage: "circle.grid.3x3") }
            SettingsView().tabItem { Label("Settings", systemImage: "gear") }
            StatusView().tabItem { Label("Status", systemImage: "antenna.radiowaves.left.and.right") }
        }
        // Ringing is CallKit's UI alone (banner, lock screen, Recents). While
        // a call is ACTIVE and the app is in front, iOS shows only the green
        // status indicator, so the in-call controls are ours. The cover
        // stands for "in a call", not for one particular call: keyed on the
        // active call's identity it was dismissed and presented again every
        // time the active call changed (accepting a second call, a swap, the
        // resume after a hang-up) — the keypad flashed through and iOS's own
        // banner blinked with each pass (device, 2026-09-14). The screen
        // inside follows `activeCall` and re-renders in place.
        .fullScreenCover(isPresented: Binding(get: { !model.calls.isEmpty }, set: { _ in })) {
            InCallView()
        }
    }
}

// MARK: - In-call

struct InCallView: View {
    @EnvironmentObject private var model: AppModel
    @State private var transferTo = ""
    @State private var showTransfer = false

    var body: some View {
        // `activeCall` is nil only while the cover animates away after the
        // last call ended; the screen keeps its frame and shows nothing.
        if let call = model.activeCall { content(for: call) }
    }

    @ViewBuilder
    private func content(for call: AppModel.ActiveCall) -> some View {
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
            if let notice = model.notice {
                Label(notice, systemImage: "exclamationmark.circle")
                    .font(.callout).foregroundStyle(.secondary).lineLimit(2)
                    .multilineTextAlignment(.center)
                    .transition(.opacity)
            }
            if let held = model.heldCall {
                // Call waiting: the other call is on hold. Switching between
                // the two is the system's job — iOS 26 shows its own Swap
                // banner over the app while a call is held — so this screen
                // only names the held party. Ending the held call is swap,
                // then End, as in the Phone app.
                Label("\(held.title) on hold", systemImage: "pause.circle")
                    .font(.callout).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer()
            HStack(spacing: 28) {
                CallButton(title: call.muted ? "Unmute" : "Mute", system: call.muted ? "mic.slash.fill" : "mic.fill", active: call.muted) { model.toggleMute() }
                if model.heldCall == nil {
                    CallButton(title: call.held ? "Resume" : "Hold", system: call.held ? "play.fill" : "pause.fill", active: call.held) { model.toggleHold() }
                } else if !Self.systemSwapsCalls {
                    // iOS 17/18 show no swap banner over a foreground app:
                    // the Hold button becomes Swap, as on the Phone app.
                    CallButton(title: "Swap", system: "arrow.left.arrow.right", active: false) { model.swapCalls() }
                }
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

// MARK: - Recents

struct RecentsView: View {
    @EnvironmentObject private var model: AppModel
    @State private var missedOnly = false
    @State private var confirmClear = false

    private var shown: [CallRecord] {
        missedOnly ? model.recents.filter(\.isMissed) : model.recents
    }

    var body: some View {
        NavigationStack {
            list
                .listStyle(.plain)
                .overlay { if shown.isEmpty { empty } }
                .navigationTitle("Recents")
                .toolbar { toolbar }
                .confirmationDialog("Clear all recent calls?", isPresented: $confirmClear, titleVisibility: .visible) {
                    Button("Clear All", role: .destructive) { model.clearRecents() }
                }
                .onAppear { model.markRecentsSeen() }
                .onChange(of: model.recents) { _, _ in model.markRecentsSeen() }
        }
    }

    private var list: some View {
        List {
            ForEach(shown) { r in
                Button {
                    model.dial(r.counterpart.uri)
                } label: {
                    RecentRow(record: r, name: model.recentName(for: r))
                }
                .disabled(model.activeCall != nil)
            }
            .onDelete(perform: delete)
        }
    }

    private func delete(at offsets: IndexSet) {
        let ids = offsets.map { shown[$0].id }
        for id in ids { model.deleteRecent(id: id) }
    }

    private var empty: some View {
        ContentUnavailableView(missedOnly ? "No missed calls" : "No recent calls", systemImage: "clock",
                               description: Text(missedOnly ? "Missed calls appear here." : "Calls you make and receive appear here."))
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .principal) {
            Picker("Filter", selection: $missedOnly) {
                Text("All").tag(false)
                Text("Missed").tag(true)
            }
            .pickerStyle(.segmented)
            .frame(maxWidth: 200)
        }
        ToolbarItem(placement: .topBarTrailing) {
            Button("Clear") { confirmClear = true }
                .disabled(model.recents.isEmpty)
        }
    }
}

private struct RecentRow: View {
    let record: CallRecord
    let name: String

    private var glyph: (name: String, color: Color) {
        switch record.outcome {
        case .missed: return ("phone.arrow.down.left", .red)
        case .declined where record.direction == .incoming: return ("phone.arrow.down.left", .secondary)
        case .answeredElsewhere: return ("phone.arrow.down.left", .secondary)
        default: return record.direction == .outgoing ? ("phone.arrow.up.right", .secondary) : ("phone.arrow.down.left", .secondary)
        }
    }

    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: glyph.name).foregroundStyle(glyph.color).frame(width: 22)
            VStack(alignment: .leading, spacing: 2) {
                Text(name).foregroundStyle(record.isMissed ? .red : .primary).lineLimit(1)
                Text(record.number).font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 2) {
                Text(record.endedAt, format: Self.when(record.endedAt)).font(.caption).foregroundStyle(.secondary)
                Text(record.summary).font(.caption).foregroundStyle(record.duration == nil ? .secondary : .primary)
            }
        }
        .contentShape(Rectangle())
    }

    /// Today: the time; this week: the weekday; otherwise the date.
    private static func when(_ date: Date) -> Date.FormatStyle {
        let cal = Calendar.current
        if cal.isDateInToday(date) { return .dateTime.hour().minute() }
        if let week = cal.date(byAdding: .day, value: -6, to: Date()), date > week { return .dateTime.weekday(.wide) }
        return .dateTime.day().month(.abbreviated)
    }
}

// MARK: - Directory

struct DirectoryView: View {
    @EnvironmentObject private var model: AppModel
    @State private var query = ""
    /// The contact being edited, or a blank one for "add".
    @State private var editing: ContactEditor.Target?

    private var shown: [DirectoryContact] { DirectorySearch.filter(model.contacts, query: query) }
    private var favourites: [DirectoryContact] { shown.filter(\.isFavourite) }

    var body: some View {
        NavigationStack {
            list
                .searchable(text: $query, prompt: "Name or extension")
                .overlay { if shown.isEmpty { empty } }
                .navigationTitle("Directory")
                .toolbar { toolbar }
                .sheet(item: $editing) { (target: ContactEditor.Target) in ContactEditor(target: target) }
                .alert("Directory", isPresented: Binding(get: { model.directoryError != nil }, set: { if !$0 { model.directoryError = nil } })) {
                    Button("OK", role: .cancel) {}
                } message: {
                    Text(model.directoryError ?? "")
                }
        }
    }

    private var list: some View {
        List {
            if !favourites.isEmpty {
                Section("Favourites") { rows(favourites) }
            }
            Section(favourites.isEmpty ? "" : "All") { rows(shown) }
        }
    }

    private func rows(_ contacts: [DirectoryContact]) -> some View {
        ForEach(contacts) { c in
            Button {
                model.dial(c.uri)
            } label: {
                ContactRow(contact: c)
            }
            .disabled(model.activeCall != nil)
            .swipeActions(edge: .leading) {
                Button { Task { await model.toggleFavourite(c) } } label: {
                    Label(c.isFavourite ? "Unstar" : "Star", systemImage: c.isFavourite ? "star.slash" : "star")
                }
                .tint(.yellow)
            }
            .swipeActions(edge: .trailing) {
                Button(role: .destructive) { Task { await model.deleteContact(id: c.id) } } label: {
                    Label("Delete", systemImage: "trash")
                }
                Button { editing = .edit(c) } label: { Label("Edit", systemImage: "pencil") }
                    .tint(.blue)
            }
        }
    }

    private var empty: some View {
        ContentUnavailableView(query.isEmpty ? "No contacts yet" : "No matches", systemImage: "person.2",
                               description: Text(query.isEmpty ? "Add one, or connect and refresh." : "Try another name or extension."))
    }

    @ToolbarContentBuilder
    private var toolbar: some ToolbarContent {
        ToolbarItem(placement: .topBarLeading) {
            Button { Task { await model.syncDirectory() } } label: { Image(systemName: "arrow.clockwise") }
        }
        ToolbarItem(placement: .topBarTrailing) {
            Button { editing = .add } label: { Image(systemName: "plus") }
        }
    }
}

private struct ContactRow: View {
    let contact: DirectoryContact

    var body: some View {
        HStack {
            VStack(alignment: .leading) {
                HStack(spacing: 4) {
                    if contact.isFavourite { Image(systemName: "star.fill").font(.caption).foregroundStyle(.yellow) }
                    Text(contact.displayName).foregroundStyle(.primary)
                }
                Text(CallController.numberPart(of: contact.uri)).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            Text(contact.mode).font(.caption2).foregroundStyle(.secondary)
            Image(systemName: "phone.fill").foregroundStyle(.green)
        }
    }
}

/// Add or edit one contact. The server is the source of truth: Save writes
/// there and the list follows by sync; nothing changes locally on failure.
struct ContactEditor: View {
    enum Target: Identifiable {
        case add
        case edit(DirectoryContact)
        var id: String {
            if case .edit(let c) = self { return c.id }
            return "add"
        }
    }

    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    let target: Target
    @State private var name = ""
    @State private var number = ""
    @State private var mode = "local"
    @State private var favourite = false
    @State private var saving = false

    private var isNew: Bool { if case .add = target { return true } else { return false } }
    private var valid: Bool { !name.trimmingCharacters(in: .whitespaces).isEmpty && !number.trimmingCharacters(in: .whitespaces).isEmpty }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField("Name", text: $name)
                    TextField("Number or SIP address", text: $number)
                        .keyboardType(.numbersAndPunctuation)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .onChange(of: number) { _, n in if isNew { mode = model.defaultMode(for: n) } }
                }
                Section {
                    Picker("Reached", selection: $mode) {
                        Text("On this server (local)").tag("local")
                        Text("Through the PBX (trunk)").tag("trunk")
                    }
                    Toggle("Favourite", isOn: $favourite)
                }
            }
            .navigationTitle(isNew ? "New Contact" : "Edit Contact")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    Button("Save") { Task { await save() } }.disabled(!valid || saving)
                }
            }
            .onAppear {
                if case .edit(let c) = target {
                    name = c.displayName
                    number = c.uri
                    mode = c.mode
                    favourite = c.isFavourite
                }
            }
        }
    }

    private func save() async {
        saving = true
        defer { saving = false }
        let draft = ContactDraft(displayName: name.trimmingCharacters(in: .whitespaces),
                                 uri: number.trimmingCharacters(in: .whitespaces), mode: mode, favourite: favourite)
        let ok: Bool
        switch target {
        case .add: ok = await model.addContact(draft)
        case .edit(let c): ok = await model.updateContact(id: c.id, draft)
        }
        if ok { dismiss() }
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
                    Button("Send diagnostics to the server") { Task { await model.sendDiagnostics(reason: "manual") } }
                    if !model.diagnosticsStatus.isEmpty {
                        Text(model.diagnosticsStatus).font(.caption).foregroundStyle(.secondary)
                    }
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
    /// Comma-separated, as typed; parsed by `SSIDList` on save. Prefilled
    /// from the saved configuration, which loads asynchronously.
    @State private var ssids = ""

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
                Section("Calls") {
                    Toggle("Call waiting", isOn: $model.callWaiting)
                    Text("On: a second caller rings while you are on a call (Hold & Accept, End & Accept or Decline). Off: a second caller hears busy.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                Section("Local Push Connectivity (device only)") {
                    TextField("Office Wi-Fi SSIDs (comma-separated)", text: $ssids)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                    Text("The provider runs, and calls reach the locked phone, whenever the phone is joined to any of these networks.")
                        .font(.caption).foregroundStyle(.secondary)
                    Button("Enable background wakeups on these SSIDs") { model.configureLocalPush(ssids: SSIDList.parse(ssids)) }
                        .disabled(SSIDList.parse(ssids).isEmpty)
                    Button("Remove saved configuration", role: .destructive) { model.removeLocalPush() }
                    LabeledContent("State", value: model.localPushStatus)
                    LabeledContent("Background calls", value: model.backgroundCalls)
                    Text("\"Background calls\" is whether iOS is running the provider right now. While it says no, a call to this phone cannot arrive unless the app is open — the server has nowhere to send the wake.")
                        .font(.caption).foregroundStyle(.secondary)
                }
                .onAppear { if ssids.isEmpty { ssids = SSIDList.format(model.localPushSSIDs) } }
                .onChange(of: model.localPushSSIDs) { _, saved in
                    if ssids.isEmpty { ssids = SSIDList.format(saved) }
                }
                Section {
                    Button("Save & connect") { model.connect() }
                }
            }
            .navigationTitle("Settings")
        }
    }
}
