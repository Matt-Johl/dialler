import DiallerCore
import SwiftUI

struct RecentsView: View {
    @EnvironmentObject private var model: AppModel
    @State private var missedOnly = false
    @State private var confirmClear = false

    private var shown: [CallRecord] {
        missedOnly ? model.recents.filter(\.isMissed) : model.recents
    }

    var body: some View {
        VStack(spacing: 0) {
            ScreenHeader(title: "Recents") {
                Picker("Show", selection: $missedOnly) {
                    Text("All").tag(false)
                    Text("Missed").tag(true)
                }
                .pickerStyle(.segmented)
                .fixedSize()
                Menu {
                    Button("Clear All Recents", systemImage: "trash", role: .destructive) { confirmClear = true }
                        .disabled(model.recents.isEmpty)
                } label: {
                    Image(systemName: "ellipsis")
                        .font(.body)
                        .foregroundStyle(Palette.ink)
                        .frame(width: 36, height: 36)
                        .overlay { Circle().strokeBorder(Palette.ring, lineWidth: 1) }
                        .frame(width: 44, height: 44)
                        .contentShape(Rectangle())
                }
                .accessibilityLabel("More")
            }
            list
        }
        .background(Palette.ground)
        .confirmationDialog("Clear all recent calls?", isPresented: $confirmClear, titleVisibility: .visible) {
            Button("Clear All Recents", role: .destructive) { model.clearRecents() }
        } message: {
            Text("Calls in the Phone app’s Recents aren’t affected.")
        }
        .onAppear { model.markRecentsSeen() }
        .onChange(of: model.recents) { _, _ in model.markRecentsSeen() }
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
                .hairlineRow()
            }
            .onDelete(perform: delete)
        }
        .listStyle(.plain)
        .scrollContentBackground(.hidden)
        .overlay { if shown.isEmpty { empty } }
    }

    private func delete(at offsets: IndexSet) {
        let ids = offsets.map { shown[$0].id }
        for id in ids { model.deleteRecent(id: id) }
    }

    private var empty: some View {
        ContentUnavailableView {
            Label(missedOnly ? "No Missed Calls" : "No Recent Calls", systemImage: missedOnly ? "phone.arrow.down.left" : "clock")
        } description: {
            Text(missedOnly ? "Calls you miss appear here." : "Calls you make and receive appear here.")
        }
        .foregroundStyle(Palette.secondary)
    }
}

private struct RecentRow: View {
    let record: CallRecord
    let name: String
    /// At the accessibility text sizes the row stacks: no monogram, and
    /// the time goes under the name instead of squeezing it.
    @Environment(\.dynamicTypeSize) private var typeSize

    /// Which way the call went, and a cross for one that was missed.
    private var glyph: String {
        if record.isMissed { return "xmark" }
        return record.direction == .outgoing ? "arrow.up.right" : "arrow.down.left"
    }

    var body: some View {
        HStack(spacing: 14) {
            if !typeSize.isAccessibilitySize { Monogram(name: name) }
            VStack(alignment: .leading, spacing: 3) {
                Text(name)
                    .font(.callout.weight(record.isMissed ? .semibold : .medium))
                    .foregroundStyle(Palette.ink)
                    .lineLimit(typeSize.isAccessibilitySize ? 3 : 1)
                HStack(alignment: .firstTextBaseline, spacing: 5) {
                    Image(systemName: glyph).font(.caption2.weight(.semibold))
                    Text(CallText.detail(for: record, name: name)).lineLimit(typeSize.isAccessibilitySize ? 3 : 1)
                }
                .font(.footnote)
                .foregroundStyle(Palette.secondary)
                if typeSize.isAccessibilitySize { time }
            }
            Spacer(minLength: 8)
            if !typeSize.isAccessibilitySize { time }
        }
        .padding(.vertical, 12)
        .contentShape(Rectangle())
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("\(name), \(accessibilityKind), \(CallText.when(record.endedAt))")
        .accessibilityHint("Calls \(name).")
    }

    private var time: some View {
        Text(CallText.when(record.endedAt))
            .font(.footnote)
            .foregroundStyle(Palette.secondary)
    }

    private var accessibilityKind: String {
        if record.isMissed { return "missed call" }
        let kind = record.direction == .outgoing ? "outgoing call" : "incoming call"
        return record.duration.map { "\(kind), \(CallText.duration($0))" } ?? "\(kind), \(record.outcome.label)"
    }
}

extension View {
    /// A list row on the ground colour, inset to the screen's margins, with
    /// a hairline that runs the full width of the text column.
    func hairlineRow() -> some View {
        self
            .listRowInsets(EdgeInsets(top: 0, leading: 24, bottom: 0, trailing: 24))
            .listRowBackground(Palette.ground)
            .listRowSeparatorTint(Palette.hairline)
            .alignmentGuide(.listRowSeparatorLeading) { _ in 0 }
            .alignmentGuide(.listRowSeparatorTrailing) { d in d.width }
    }
}
