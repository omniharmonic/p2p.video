import AppKit
import SwiftUI

/// The app's look, matched to the web redesign (docs/design/UX_REDESIGN.md):
/// follows the system appearance, teal accent, pill-shaped primary actions,
/// SF Symbols. Native controls everywhere else [SL-UX-2].
enum Theme {
    /// Signal teal: #0F7B76 in light, #5FCFC6 in dark.
    static let accent = Color(nsColor: dynamic(light: 0x0F7B76, dark: 0x5FCFC6))
    /// Text on a filled accent pill (white on dark teal, ink on light teal), 4.5:1 or better.
    static let onAccent = Color(nsColor: dynamic(light: 0xFFFFFF, dark: 0x101418))
    /// Soft accent wash for selected rows and chips.
    static let accentSoft = Color(nsColor: dynamic(light: 0x0F7B76, dark: 0x5FCFC6, alpha: 0.13))
    /// Cards: white on the light canvas, raised surface in dark.
    static let surface = Color(nsColor: dynamic(light: 0xFFFFFF, dark: 0x171C21))
    static let canvas = Color(nsColor: dynamic(light: 0xF4F6F5, dark: 0x101418))

    private static func dynamic(light: UInt32, dark: UInt32, alpha: CGFloat = 1) -> NSColor {
        NSColor(name: nil) { appearance in
            let isDark = appearance.bestMatch(from: [.aqua, .darkAqua]) == .darkAqua
            return rgb(isDark ? dark : light, alpha: alpha)
        }
    }

    private static func rgb(_ hex: UInt32, alpha: CGFloat) -> NSColor {
        NSColor(srgbRed: CGFloat((hex >> 16) & 0xFF) / 255, green: CGFloat((hex >> 8) & 0xFF) / 255,
                blue: CGFloat(hex & 0xFF) / 255, alpha: alpha)
    }
}

/// Rounded pill buttons: `.prominent` is the one teal primary action of a
/// view; `.quiet` is the outlined secondary.
struct PillButtonStyle: ButtonStyle {
    enum Kind { case prominent, quiet }
    var kind: Kind = .prominent
    var large = true

    func makeBody(configuration: Configuration) -> some View {
        PillBody(configuration: configuration, kind: kind, large: large)
    }

    private struct PillBody: View {
        let configuration: Configuration
        let kind: Kind
        let large: Bool
        @Environment(\.isEnabled) private var enabled

        var body: some View {
            configuration.label
                .font(large ? .body.weight(.semibold) : .callout.weight(.medium))
                .lineLimit(1)
                .padding(.horizontal, large ? 18 : 12)
                .padding(.vertical, large ? 7 : 4)
                .foregroundStyle(kind == .prominent ? Theme.onAccent : Theme.accent)
                .background {
                    if kind == .prominent {
                        Capsule().fill(Theme.accent)
                    } else {
                        // strokeBorder draws inside the shape, so the outline is never
                        // clipped at the capsule's ends.
                        ZStack {
                            Capsule().fill(Theme.accentSoft.opacity(configuration.isPressed ? 1 : 0.5))
                            Capsule().strokeBorder(Theme.accent.opacity(0.5), lineWidth: 1)
                        }
                    }
                }
                .contentShape(Capsule())
                .opacity(enabled ? (configuration.isPressed ? 0.8 : 1) : 0.45)
        }
    }
}

extension ButtonStyle where Self == PillButtonStyle {
    static var pill: PillButtonStyle { PillButtonStyle() }
    static var quietPill: PillButtonStyle { PillButtonStyle(kind: .quiet) }
    static var smallPill: PillButtonStyle { PillButtonStyle(large: false) }
    static var smallQuietPill: PillButtonStyle { PillButtonStyle(kind: .quiet, large: false) }
}

/// A disclosure that keeps technical detail one click away ("Show details").
struct DetailsDisclosure<Content: View>: View {
    var label = "Show details"
    @ViewBuilder var content: Content
    @State private var open = false

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Button {
                withAnimation(.easeInOut(duration: 0.15)) { open.toggle() }
            } label: {
                HStack(spacing: 4) {
                    Image(systemName: "chevron.right")
                        .font(.caption.weight(.semibold))
                        .rotationEffect(.degrees(open ? 90 : 0))
                    Text(open ? label.replacingOccurrences(of: "Show", with: "Hide") : label)
                }
                .font(.callout)
                .foregroundStyle(.secondary)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityAddTraits(.isButton)
            .accessibilityValue(open ? "expanded" : "collapsed")
            if open { content }
        }
    }
}

/// Exact commands, monospaced and selectable.
struct CommandList: View {
    let commands: [String]

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            ForEach(commands, id: \.self) { c in
                Text(c).font(.caption.monospaced()).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 8, style: .continuous).fill(.quaternary.opacity(0.6)))
    }
}
