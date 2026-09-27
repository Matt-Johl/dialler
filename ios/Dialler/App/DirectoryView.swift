import DiallerCore
import SwiftUI

struct DirectoryView: View {
    @EnvironmentObject private var model: AppModel
    @State private var query = ""
    /// The contact being edited, or a blank one for "add".
    @State private var editing: ContactEditor.Target?

    private var shown: [DirectoryContact] { DirectorySearch.filter(model.contacts, query: query) }
    private var favourites: [DirectoryContact] { query.isEmpty ? shown.filter(\.isFavourite) : [] }

    /// The list under the favourites, a section per initial letter; names
    /// that start with anything else share "#", last.
    private var groups: [(letter: String, contacts: [DirectoryContact])] {
        let byLetter = Dictionary(grouping: shown) { c -> String in
            guard let first = c.displayName.first, first.isLetter else { return "#" }
            return String(first).folding(options: [.diacriticInsensitive, .caseInsensitive], locale: .current).uppercased()
        }
        return byLetter.keys.sorted { a, b in a == "#" ? false : b == "#" ? true : a < b }.map { ($0, byLetter[$0]!) }
    }

    var body: some View {
        VStack(spacing: 0) {
            ScreenHeader(title: "Directory") {
                Button { editing = .add } label: {
                    Image(systemName: "plus")
                        .font(.body)
                        .foregroundStyle(Palette.ink)
                        .frame(width: 36, height: 36)
                        .overlay { Circle().strokeBorder(Palette.ring, lineWidth: 1) }
                        .frame(width: 44, height: 44)
                        .contentShape(Rectangle())
                }
                .accessibilityLabel("Add Contact")
            }
            SearchField(text: $query, prompt: "Search name or number")
                .padding(.horizontal, 24)
                .padding(.bottom, 6)
            list
        }
        .background(Palette.ground)
        .sheet(item: $editing) { (target: ContactEditor.Target) in ContactEditor(target: target) }
        .alert("Couldn’t Update the Directory", isPresented: Binding(get: { model.directoryError != nil }, set: { if !$0 { model.directoryError = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(model.directoryError ?? "")
        }
    }

    private var list: some View {
        List {
            if !favourites.isEmpty {
                Section {
                    rows(favourites, inFavourites: true)
                } header: {
                    header("Favourites")
                }
            }
            ForEach(groups, id: \.letter) { group in
                Section {
                    rows(group.contacts, inFavourites: false)
                } header: {
                    header(group.letter)
                }
            }
        }
        .listStyle(.plain)
        .listSectionSpacing(0)
        .environment(\.defaultMinListHeaderHeight, 0)
        .scrollContentBackground(.hidden)
        .scrollDismissesKeyboard(.immediately)
        .refreshable { await model.syncDirectory() }
        .overlay { if shown.isEmpty { empty } }
    }

    private func header(_ text: String) -> some View {
        SectionLabel(text: text)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.horizontal, 24)
            .padding(.top, 16)
            .padding(.bottom, 4)
            .background(Palette.ground)
            .listRowInsets(EdgeInsets())
    }

    private func rows(_ contacts: [DirectoryContact], inFavourites: Bool) -> some View {
        ForEach(contacts) { c in
            Button {
                model.dial(c.uri)
            } label: {
                ContactRow(contact: c, showsStar: !inFavourites)
            }
            .disabled(model.activeCall != nil)
            .hairlineRow()
            .swipeActions(edge: .leading) {
                Button { Task { await model.toggleFavourite(c) } } label: {
                    Label(c.isFavourite ? "Unfavourite" : "Favourite", systemImage: c.isFavourite ? "star.slash" : "star")
                }
                .tint(.orange)
            }
            .swipeActions(edge: .trailing) {
                Button(role: .destructive) { Task { await model.deleteContact(id: c.id) } } label: {
                    Label("Delete", systemImage: "trash")
                }
                Button { editing = .edit(c) } label: { Label("Edit", systemImage: "pencil") }
                    .tint(.gray)
            }
            .contextMenu {
                Button("Call", systemImage: "phone") { model.dial(c.uri) }
                    .disabled(model.activeCall != nil)
                Button(c.isFavourite ? "Remove from Favourites" : "Add to Favourites", systemImage: c.isFavourite ? "star.slash" : "star") {
                    Task { await model.toggleFavourite(c) }
                }
                Button("Edit Contact", systemImage: "pencil") { editing = .edit(c) }
                Button("Delete Contact", systemImage: "trash", role: .destructive) { Task { await model.deleteContact(id: c.id) } }
            }
        }
    }

    @ViewBuilder
    private var empty: some View {
        if query.isEmpty {
            ContentUnavailableView {
                Label("No Contacts", systemImage: "person.2")
            } description: {
                Text("Add a contact, or pull down to refresh.")
            }
            .foregroundStyle(Palette.secondary)
        } else {
            ContentUnavailableView.search(text: query)
        }
    }
}

/// The canvas's search field: a quiet fill, a glass, a clear button.
struct SearchField: View {
    @Binding var text: String
    let prompt: String

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: "magnifyingglass").foregroundStyle(Palette.tertiary)
            TextField("Search", text: $text, prompt: Text(prompt).foregroundStyle(Palette.tertiary))
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
                .submitLabel(.search)
                .foregroundStyle(Palette.ink)
            if !text.isEmpty {
                Button { text = "" } label: {
                    Image(systemName: "xmark.circle.fill").foregroundStyle(Palette.tertiary)
                }
                .accessibilityLabel("Clear Search")
            }
        }
        .font(.subheadline)
        .padding(.horizontal, 14)
        .frame(height: 44)
        .background(Palette.fill, in: RoundedRectangle(cornerRadius: 12, style: .continuous))
    }
}

private struct ContactRow: View {
    let contact: DirectoryContact
    /// A star beside a favourite, except in the Favourites section itself.
    let showsStar: Bool
    @Environment(\.dynamicTypeSize) private var typeSize

    var body: some View {
        HStack(spacing: 14) {
            if !typeSize.isAccessibilitySize { Monogram(name: contact.displayName) }
            VStack(alignment: .leading, spacing: 3) {
                Text(contact.displayName)
                    .font(.callout.weight(.medium))
                    .foregroundStyle(Palette.ink)
                    .lineLimit(typeSize.isAccessibilitySize ? 3 : 1)
                Text(CallText.party(CallController.numberPart(of: contact.uri)))
                    .font(.footnote)
                    .foregroundStyle(Palette.secondary)
                    .lineLimit(1)
            }
            Spacer(minLength: 8)
            if showsStar && contact.isFavourite {
                Image(systemName: "star.fill")
                    .font(.caption)
                    .foregroundStyle(Palette.tertiary)
                    .accessibilityLabel("Favourite")
            }
        }
        .padding(.vertical, 11)
        .contentShape(Rectangle())
        .accessibilityElement(children: .combine)
        .accessibilityHint("Calls \(contact.displayName).")
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
                        .textContentType(.name)
                    TextField("Number or SIP Address", text: $number)
                        .keyboardType(.numbersAndPunctuation)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .onChange(of: number) { _, n in if isNew { mode = model.defaultMode(for: n) } }
                }
                Section {
                    Picker("Route", selection: $mode) {
                        Text("Direct").tag("local")
                        Text("Phone System").tag("trunk")
                    }
                } footer: {
                    Text("Direct reaches another \(AppName.display) on your server. Phone System goes through your office’s phone system (PBX).")
                }
                Section {
                    Toggle("Favourite", isOn: $favourite)
                }
            }
            .navigationTitle(isNew ? "New Contact" : "Edit Contact")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                ToolbarItem(placement: .confirmationAction) {
                    if saving {
                        ProgressView()
                    } else {
                        Button(isNew ? "Add" : "Done") { Task { await save() } }
                            .fontWeight(.semibold)
                            .disabled(!valid)
                    }
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
        .tint(Palette.ink)
        .interactiveDismissDisabled(saving)
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
