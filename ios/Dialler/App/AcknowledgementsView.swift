import SwiftUI

/// Settings › About › Acknowledgements: the open-source software built into
/// the app and its licences, which require their notices to ship with it
/// (appstore.md, must-fix 5). The texts are bundled verbatim from the
/// vendored sources (App/Licences).
struct AcknowledgementsView: View {
    struct Component: Identifiable, Hashable {
        let name: String
        let version: String
        let licence: String
        /// Bundled `Licence-<file>.txt`.
        let file: String
        var id: String { file }
    }

    /// The versions ios/vendor/build-baresip.sh builds; update both together.
    static let components = [
        Component(name: "baresip", version: "3.15.0", licence: "BSD 3-Clause", file: "baresip"),
        Component(name: "G.722 codec", version: "SpanDSP", licence: "Public domain", file: "G722"),
        Component(name: "libre", version: "3.15.0", licence: "BSD 3-Clause", file: "libre"),
        Component(name: "OpenSSL", version: "3.3.2", licence: "Apache 2.0", file: "OpenSSL"),
        Component(name: "Opus", version: "1.5.2", licence: "BSD 3-Clause", file: "Opus"),
    ]

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                SettingsFooter("\(AppName.display) includes the following open-source software.")
                    .padding(.bottom, 8)
                ForEach(Self.components) { c in
                    NavigationLink(value: c) {
                        HStack(alignment: .firstTextBaseline, spacing: 16) {
                            VStack(alignment: .leading, spacing: 3) {
                                Text(c.name).font(.callout).foregroundStyle(Palette.ink)
                                Text(c.version).font(.footnote).foregroundStyle(Palette.secondary)
                            }
                            Spacer(minLength: 8)
                            Text(c.licence).font(.subheadline).foregroundStyle(Palette.secondary)
                            Image(systemName: "chevron.right")
                                .font(.footnote.weight(.semibold))
                                .foregroundStyle(Palette.tertiary)
                        }
                        .padding(.vertical, 12)
                        .frame(minHeight: 52)
                        .overlay(alignment: .bottom) { Palette.hairline.frame(height: 1) }
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                }
            }
            .padding(.horizontal, 24)
            .padding(.bottom, 32)
        }
        .background(Palette.ground)
        .navigationTitle("Acknowledgements")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
        .navigationDestination(for: Component.self) { LicenceView(component: $0) }
    }
}

/// One component's licence, as shipped with its source.
private struct LicenceView: View {
    let component: AcknowledgementsView.Component

    private var text: String {
        Bundle.main.url(forResource: "Licence-\(component.file)", withExtension: "txt")
            .flatMap { try? String(contentsOf: $0, encoding: .utf8) }
            .map(Self.reflow)
            ?? "The licence text is missing from this build."
    }

    /// The files are hard-wrapped for an 80-column terminal; on a phone
    /// those breaks land mid-line. Each paragraph becomes one run of text,
    /// and the blank lines between paragraphs stay.
    static func reflow(_ text: String) -> String {
        text.replacingOccurrences(of: "\r\n", with: "\n")
            .components(separatedBy: "\n\n")
            .map { para in
                para.split(separator: "\n")
                    .map { $0.trimmingCharacters(in: .whitespaces) }
                    .filter { !$0.isEmpty }
                    .joined(separator: " ")
            }
            .filter { !$0.isEmpty }
            .joined(separator: "\n\n")
    }

    var body: some View {
        ScrollView {
            Text(text)
                .font(.footnote)
                .foregroundStyle(Palette.secondary)
                .textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 24)
                .padding(.vertical, 16)
        }
        .background(Palette.ground)
        .navigationTitle(component.name)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar(.visible, for: .navigationBar)
    }
}
