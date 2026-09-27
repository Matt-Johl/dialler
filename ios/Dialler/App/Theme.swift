import DiallerCore
import SwiftUI
import UIKit

/// The app's look ("Hairline", from the design canvas): a near-white
/// ground, near-black ink, hairline rules and outlined circles, one weight
/// of accent — the ink itself. Every colour has a dark counterpart.
enum Palette {
    /// Screen background.
    static let ground = Color(light: 0xFBFBFC, dark: 0x0A0B0F)
    /// Primary text, filled buttons, the selected tab.
    static let ink = Color(light: 0x0B0D12, dark: 0xF4F5F7)
    /// Secondary text: subtitles, values, captions.
    static let secondary = Color(light: 0x5A606C, dark: 0x9AA0AB)
    /// Placeholders and idle glyphs.
    static let tertiary = Color(light: 0x6B717D, dark: 0x7C828E)
    /// Row separators.
    static let hairline = Color(light: 0xECEEF1, dark: 0x1C1F26)
    /// The outline of avatars, keys and rings.
    static let ring = Color(light: 0xE2E5EA, dark: 0x2A2D36)
    /// Quiet fills: the search field, secondary buttons, a pressed key.
    static let fill = Color(light: 0xEEF0F3, dark: 0x16181E)
    /// A raised surface on a fill: the selected segment.
    static let raised = Color(light: 0xFFFFFF, dark: 0x2A2D36)
    /// A switch that is on: the ink in light mode; in dark mode a grey, since
    /// near-white would hide the switch's white knob.
    static let switchOn = Color(light: 0x0B0D12, dark: 0x8A909C)
    /// Ending a call. The one colour that is not ink.
    static let end = Color(light: 0xE5484D, dark: 0xE5484D)
}

extension Color {
    init(light: UInt32, dark: UInt32) {
        self.init(UIColor { $0.userInterfaceStyle == .dark ? UIColor(rgb: dark) : UIColor(rgb: light) })
    }
}

extension UIColor {
    convenience init(rgb: UInt32) {
        self.init(red: CGFloat(rgb >> 16 & 0xFF) / 255, green: CGFloat(rgb >> 8 & 0xFF) / 255,
                  blue: CGFloat(rgb & 0xFF) / 255, alpha: 1)
    }
}

/// The app's name as the Home Screen shows it (Info.plist), so no screen
/// can disagree with it.
enum AppName {
    static let display = Bundle.main.object(forInfoDictionaryKey: "CFBundleDisplayName") as? String ?? "Dialler"
}

/// The Dialler mark: a line with stepped bars either side, turned 45°.
/// The same five bars as the admin console's logo and the app icon
/// (server/internal/adminui/static/logo-light.svg), in its 1024-unit box,
/// so it fills with whatever colour the view gives it.
struct DiallerMark: Shape {
    private static let bars = [
        CGRect(x: -500, y: -35, width: 1000, height: 70),
        CGRect(x: -450, y: -140, width: 340, height: 70),
        CGRect(x: -390, y: -245, width: 200, height: 70),
        CGRect(x: 110, y: 70, width: 340, height: 70),
        CGRect(x: 190, y: 175, width: 200, height: 70),
    ]

    func path(in rect: CGRect) -> Path {
        let scale = min(rect.width, rect.height) / 1024
        var path = Path()
        for bar in Self.bars { path.addRect(bar) }
        return path.applying(CGAffineTransform(translationX: rect.midX, y: rect.midY)
            .scaledBy(x: scale, y: scale)
            .rotated(by: -.pi / 4))
    }
}

// MARK: - Screen furniture

/// A tab's title, drawn by the screen rather than the navigation bar: the
/// canvas's light large title, with room on the right for the screen's
/// own controls.
struct ScreenHeader<Trailing: View>: View {
    let title: String
    @ViewBuilder var trailing: Trailing

    var body: some View {
        HStack(alignment: .center, spacing: 12) {
            Text(title)
                .font(.largeTitle.weight(.regular))
                .tracking(-1.1)
                .foregroundStyle(Palette.ink)
                .accessibilityAddTraits(.isHeader)
            Spacer(minLength: 0)
            trailing
        }
        .padding(.horizontal, 24)
        .padding(.top, 14)
        .padding(.bottom, 10)
    }
}

extension ScreenHeader where Trailing == EmptyView {
    init(title: String) {
        self.init(title: title) { EmptyView() }
    }
}

/// A section label: small capitals, widely tracked.
struct SectionLabel: View {
    let text: String

    var body: some View {
        Text(text.uppercased())
            .font(.caption2.weight(.medium))
            .tracking(1.6)
            .foregroundStyle(Palette.secondary)
            .accessibilityAddTraits(.isHeader)
    }
}

/// Initials in an outlined circle, or a glyph when there is no name.
struct Monogram: View {
    let name: String
    var size: CGFloat = 40
    var emphasised = false

    var body: some View {
        ZStack {
            Circle().strokeBorder(emphasised ? Palette.ink : Palette.ring, lineWidth: emphasised ? 1.25 : 1)
            if let initials = CallText.initials(name) {
                Text(initials)
                    .font(.system(size: size * 0.33, weight: .medium))
                    .tracking(0.3)
                    .foregroundStyle(Palette.ink)
            } else {
                Image(systemName: "phone")
                    .font(.system(size: size * 0.36, weight: .light))
                    .foregroundStyle(Palette.secondary)
            }
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }
}

/// The canvas's concentric hairline rings around a centre: the status
/// glyph on Settings, the caller on the in-call screen, the onboarding mark.
struct Rings<Centre: View>: View {
    var size: CGFloat = 168
    var inner: CGFloat = 0.43
    var innerStroke: Color = Palette.ink
    @ViewBuilder var centre: Centre

    var body: some View {
        ZStack {
            Circle().strokeBorder(Palette.hairline, lineWidth: 1)
            Circle().strokeBorder(Palette.ring, lineWidth: 1).padding(size * 0.14)
            Circle().strokeBorder(innerStroke, lineWidth: 1.25).frame(width: size * inner, height: size * inner)
            centre
        }
        .frame(width: size, height: size)
        .accessibilityHidden(true)
    }
}

// MARK: - Buttons

/// Full-width buttons from the onboarding canvas: ink-filled for the main
/// action, a quiet fill for the other.
struct WideButtonStyle: ButtonStyle {
    enum Kind { case primary, secondary, outline }
    var kind: Kind = .primary
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.body.weight(.medium))
            .frame(maxWidth: .infinity, minHeight: 54)
            .foregroundStyle(kind == .primary ? Palette.ground : Palette.ink)
            .background {
                RoundedRectangle(cornerRadius: 12, style: .continuous)
                    .fill(kind == .primary ? Palette.ink : kind == .secondary ? Palette.fill : Color.clear)
            }
            .overlay {
                if kind == .outline {
                    RoundedRectangle(cornerRadius: 12, style: .continuous).strokeBorder(Palette.ring, lineWidth: 1)
                }
            }
            .opacity(configuration.isPressed ? 0.7 : isEnabled ? 1 : 0.4)
            .contentShape(RoundedRectangle(cornerRadius: 12, style: .continuous))
    }
}

/// A round control: outlined when idle, filled with ink when on.
struct CircleButtonStyle: ButtonStyle {
    var size: CGFloat = 76
    var on = false
    var fill: Color?
    @Environment(\.isEnabled) private var isEnabled

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .foregroundStyle(fill != nil ? Color.white : on ? Palette.ground : Palette.ink)
            .frame(width: size, height: size)
            .background {
                Circle().fill(fill ?? (on ? Palette.ink : configuration.isPressed ? Palette.fill : Color.clear))
            }
            .overlay {
                if fill == nil && !on { Circle().strokeBorder(Palette.ring, lineWidth: 1) }
            }
            .opacity(fill != nil && configuration.isPressed ? 0.75 : isEnabled ? 1 : 0.35)
            .contentShape(Circle())
    }
}
