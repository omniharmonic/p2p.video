import AppKit
import StudioLinkCore
import SwiftUI

/// Development aid: renders app views to PNG without Screen Recording
/// permission (works with the screen locked). Enabled only by
/// `STUDIOLINK_SNAPSHOT_DIR`; see docs/DESKTOP_APP.md.
@MainActor
enum Snapshotter {
    static func capture<V: View>(_ view: V, size: CGSize, dark: Bool, to url: URL) -> Bool {
        let hosting = NSHostingView(rootView: view.frame(width: size.width, height: size.height))
        hosting.frame = NSRect(origin: .zero, size: size)
        let window = NSWindow(contentRect: hosting.frame, styleMask: [.titled], backing: .buffered, defer: false)
        window.appearance = NSAppearance(named: dark ? .darkAqua : .aqua)
        window.contentView = hosting
        window.setFrameOrigin(NSPoint(x: -10_000, y: -10_000)) // off any visible screen area
        window.orderFrontRegardless()
        hosting.layoutSubtreeIfNeeded()
        hosting.displayIfNeeded()
        defer { window.orderOut(nil) }
        guard let rep = hosting.bitmapImageRepForCachingDisplay(in: hosting.bounds) else { return false }
        hosting.cacheDisplay(in: hosting.bounds, to: rep)
        guard let png = rep.representation(using: .png, properties: [:]) else { return false }
        do {
            try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            try png.write(to: url)
            return true
        } catch {
            return false
        }
    }

    static func captureAll(model: AppModel, directory: URL) {
        var shots: [(String, AnyView, CGSize)] = [
            ("macapp-control-center", AnyView(ControlCenterView(model: model)), CGSize(width: 760, height: 560)),
            // The whole control center, including the podcast-day checklist [SL-P2-2].
            ("macapp2-control-center-full", AnyView(ControlCenterView(model: model)), CGSize(width: 760, height: 1040)),
            ("macapp-settings-guest-access", AnyView(GuestAccessPane(model: model)), CGSize(width: 640, height: 600)),
            ("macapp-settings-general", AnyView(GeneralPane(model: model)), CGSize(width: 640, height: 720)),
            ("macapp-menu", AnyView(MenuPreview(model: model)), CGSize(width: 320, height: 380)),
        ]
        for step in OnboardingStep.allCases {
            shots.append(("macapp2-onboarding-\(step.rawValue + 1)-\(step)", AnyView(OnboardingView(model: model, initialStep: step)), CGSize(width: 780, height: 560)))
        }
        // The confirm sheet for "Let guests join from anywhere" [SL-UX-2].
        if let plan = model.tailscale.plan(.enablePublic) {
            shots.append(("macapp3-confirm-public", AnyView(ConfirmPlanSheet(plan: plan, model: model) {}), CGSize(width: 560, height: 340)))
        }
        // The approval step with a sample link (no Tailscale command runs).
        let sample = TailscaleApproval(feature: .funnel, url: URL(string: "https://login.tailscale.com/f/funnel?node=nSAMPLE")!)
        shots.append(("macapp3-approval-card",
                      AnyView(ApprovalCard(approval: sample) {}.padding(20).frame(maxHeight: .infinity, alignment: .top).background(.background)),
                      CGSize(width: 560, height: 250)))
        for (name, view, size) in shots {
            for dark in [false, true] {
                let url = directory.appendingPathComponent("\(name)\(dark ? "-dark" : "").png")
                let ok = capture(view, size: size, dark: dark, to: url)
                appLog.notice("Snapshot \(url.lastPathComponent, privacy: .public): \(ok ? "written" : "failed", privacy: .public)")
            }
        }
        // `cacheDisplay` draws stroked capsules with clipped ends (a plain
        // Text + Capsule().strokeBorder shows it too), so pure-SwiftUI views
        // with outlined pills are also rendered by SwiftUI's own rasterizer
        // (`-swiftui.png`), which matches what the window shows.
        var swiftUIShots: [(String, AnyView, CGSize)] = [
            ("macapp3-approval-card", AnyView(ApprovalCard(approval: sample) {}.padding(20).frame(maxHeight: .infinity, alignment: .top)),
             CGSize(width: 560, height: 250)),
        ]
        if let plan = model.tailscale.plan(.enablePublic) {
            swiftUIShots.append(("macapp3-confirm-public", AnyView(ConfirmPlanSheet(plan: plan, model: model) {}), CGSize(width: 560, height: 340)))
        }
        for (name, view, size) in swiftUIShots {
            for dark in [false, true] {
                let url = directory.appendingPathComponent("\(name)\(dark ? "-dark" : "")-swiftui.png")
                let ok = render(view, size: size, dark: dark, to: url)
                appLog.notice("Snapshot \(url.lastPathComponent, privacy: .public): \(ok ? "written" : "failed", privacy: .public)")
            }
        }
    }

    /// SwiftUI's own rasterizer. Native AppKit controls (Picker, TextField,
    /// Form) don't render here, so only pure-SwiftUI views use it.
    static func render<V: View>(_ view: V, size: CGSize, dark: Bool, to url: URL) -> Bool {
        let content = view
            .frame(width: size.width, height: size.height)
            .background(Color(nsColor: .windowBackgroundColor))
            .environment(\.colorScheme, dark ? .dark : .light)
        let renderer = ImageRenderer(content: content)
        renderer.scale = 2
        var image: CGImage?
        // Dynamic NSColors (Theme) resolve against the current drawing appearance.
        NSAppearance(named: dark ? .darkAqua : .aqua)?.performAsCurrentDrawingAppearance { image = renderer.cgImage }
        guard let image, let png = NSBitmapImageRep(cgImage: image).representation(using: .png, properties: [:]) else { return false }
        do {
            try FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
            try png.write(to: url)
            return true
        } catch {
            return false
        }
    }
}

/// A static rendering of the menu bar menu's items for snapshots (real menus
/// can't be rendered offscreen).
struct MenuPreview: View {
    @ObservedObject var model: AppModel

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Image(systemName: model.menuBarSymbol)
                Text("p2p.video menu").font(.caption).foregroundStyle(.secondary)
            }
            Divider()
            Text(model.activityLine ?? model.statusLine).foregroundStyle(.secondary)
            Text(model.tailscale.summary).foregroundStyle(.secondary)
            Divider()
            Text("Start a Meeting        ⌘N")
            Text("Open p2p.video        ⌘O")
            if !model.calendar.upcoming.isEmpty {
                Text("Upcoming meetings").font(.caption).foregroundStyle(.secondary)
                ForEach(model.calendar.upcoming.prefix(5)) { m in
                    Text("\(m.start.formatted(date: .omitted, time: .shortened))  \(m.title)")
                }
            }
            Text("Rooms ▸").foregroundStyle(model.rooms.isEmpty ? .tertiary : .primary)
            Divider()
            Text("Open Home in Browser")
            Text("Settings…               ⌘,")
            Divider()
            Text("Quit p2p.video         ⌘Q")
        }
        .padding(14)
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .background(.regularMaterial)
    }
}
