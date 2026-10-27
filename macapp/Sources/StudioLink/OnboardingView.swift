import AppKit
import StudioLinkArtwork
import StudioLinkCore
import SwiftUI

/// First-run setup guide [SL-P2-2, SL-UX-2]: welcome, Tailscale, how guests
/// join, browser, first room, done. Every step can be skipped; Tailscale
/// changes still go through the confirm sheet (exact commands under "Show
/// details"). Re-open it from Settings → General.
struct OnboardingView: View {
    @ObservedObject var model: AppModel
    @State private var flow: OnboardingFlow
    @Environment(\.dismissWindow) private var dismissWindow

    init(model: AppModel, initialStep: OnboardingStep = .welcome) {
        self.model = model
        _flow = State(initialValue: OnboardingFlow(step: initialStep))
    }

    private var context: OnboardingContext { model.onboardingContext }

    var body: some View {
        HStack(spacing: 0) {
            sidebar
            Divider()
            VStack(spacing: 0) {
                ScrollView {
                    page
                        .frame(maxWidth: .infinity, alignment: .leading)
                        .padding(.horizontal, 36)
                        .padding(.top, 32)
                        .padding(.bottom, 16)
                }
                Divider()
                footer
            }
            .background(.background)
        }
        .frame(minWidth: 760, minHeight: 540)
        .tint(Theme.accent)
    }

    // MARK: chrome

    private var sidebar: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 10) {
                AppIconView(size: 36)
                VStack(alignment: .leading, spacing: 0) {
                    Text("p2p.video").font(.headline)
                    Text("Setup").font(.caption).foregroundStyle(.secondary)
                }
            }
            .padding(.bottom, 20)
            ForEach(OnboardingStep.allCases, id: \.self) { step in
                Button { flow.go(to: step) } label: { sidebarRow(step) }
                    .buttonStyle(.plain)
                    .disabled(step > flow.step)
            }
            Spacer()
            if !flow.isLast {
                Button("Skip Setup") { finish(open: false) }
                    .buttonStyle(.link)
                    .font(.callout)
                    .help("You can reopen this guide from Settings → General.")
            }
        }
        .padding(20)
        .frame(width: 210, alignment: .leading)
        .frame(maxHeight: .infinity, alignment: .top)
        .background(Theme.canvas)
    }

    private func sidebarRow(_ step: OnboardingStep) -> some View {
        let current = step == flow.step
        let passed = step < flow.step
        let done = passed && OnboardingFlow.satisfied(step, context)
        return HStack(spacing: 10) {
            ZStack {
                Circle().fill(current ? Theme.accent : (done ? Theme.accentSoft : Color.secondary.opacity(0.15)))
                if done {
                    Image(systemName: "checkmark").font(.system(size: 10, weight: .bold)).foregroundStyle(Theme.accent)
                } else {
                    Text("\(step.rawValue + 1)").font(.system(size: 11, weight: .semibold))
                        .foregroundStyle(current ? Theme.onAccent : Color.secondary)
                }
            }
            .frame(width: 22, height: 22)
            Text(step.title)
                .font(.callout.weight(current ? .semibold : .regular))
                .foregroundStyle(step > flow.step ? .tertiary : .primary)
            Spacer(minLength: 0)
        }
        .padding(.vertical, 6)
        .padding(.horizontal, 8)
        .background(Capsule().fill(current ? Theme.accentSoft : .clear))
        .contentShape(Rectangle())
        .accessibilityLabel("Step \(step.rawValue + 1): \(step.title)\(done ? ", done" : "")")
    }

    private var footer: some View {
        HStack(spacing: 12) {
            Text("Step \(flow.position) of \(OnboardingFlow.count)").font(.caption).foregroundStyle(.secondary)
            Spacer()
            if !flow.isFirst {
                Button("Back") { flow.back() }
                    .buttonStyle(.quietPill)
                    .keyboardShortcut(.cancelAction)
            }
            Button(OnboardingFlow.primaryLabel(flow.step, context)) { primary() }
                .buttonStyle(.pill)
                .keyboardShortcut(.defaultAction)
        }
        .padding(.horizontal, 24)
        .padding(.vertical, 14)
    }

    private func primary() {
        if flow.isLast {
            finish(open: model.supervisor.isHealthy)
        } else {
            flow.next()
        }
    }

    private func finish(open: Bool) {
        flow.finish()
        model.completeOnboarding(openDashboard: open)
        dismissWindow(id: "onboarding")
    }

    @ViewBuilder
    private var page: some View {
        switch flow.step {
        case .welcome: WelcomeStep()
        case .tailscale: TailscaleStep(model: model)
        case .guestAccess: GuestAccessStep(model: model)
        case .browser: BrowserStep(model: model)
        case .firstRoom: FirstRoomStep(model: model)
        case .done: DoneStep(model: model)
        }
    }
}

// MARK: pages

private struct StepHeader: View {
    let title: String
    let subtitle: String

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Text(title).font(.title.weight(.semibold))
            Text(subtitle).font(.body).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
        .padding(.bottom, 20)
    }
}

private struct WelcomeStep: View {
    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .center, spacing: 18) {
                AppIconView(size: 88)
                VStack(alignment: .leading, spacing: 6) {
                    Text("Welcome to p2p.video").font(.largeTitle.weight(.semibold))
                    Text("Video calls with studio-quality recording, running on your own Mac.")
                        .font(.title3).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
            }
            .padding(.bottom, 28)
            Feature(symbol: "person.2.wave.2", title: "Guests just click a link",
                    text: "Nothing to install. Guests open your link in Chrome or Safari, and you let them in.")
            Feature(symbol: "waveform.badge.mic", title: "Every voice in full quality",
                    text: "When you record, each person is recorded on their own device. A shaky connection affects the call, not the recording.")
            Feature(symbol: "lock.shield", title: "Stays on your Mac",
                    text: "No cloud account and no subscription. Meetings, rooms and recordings live on this Mac.")
            Text("Setup takes about five minutes. You can skip any step and change it later.")
                .font(.callout).foregroundStyle(.secondary).padding(.top, 8)
        }
    }
}

private struct Feature: View {
    let symbol: String
    let title: String
    let text: String

    var body: some View {
        HStack(alignment: .top, spacing: 14) {
            Image(systemName: symbol).font(.title2).foregroundStyle(Theme.accent).frame(width: 32)
            VStack(alignment: .leading, spacing: 3) {
                Text(title).font(.headline)
                Text(text).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.bottom, 16)
    }
}

private struct TailscaleStep: View {
    @ObservedObject var model: AppModel
    private var ts: TailscaleSetup { model.tailscale }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            StepHeader(title: "Connect Tailscale",
                       subtitle: "Tailscale lets guests reach this Mac safely. It’s free for personal use. Only meeting with people at this Mac? Skip this step.")
            VStack(alignment: .leading, spacing: 14) {
                CheckRow(ok: ts.cli != nil, title: "Install Tailscale",
                         detail: ts.cli != nil ? "Tailscale is installed." : "Download Tailscale for Mac, open it and follow its setup.")
                CheckRow(ok: ts.status?.loggedIn == true, title: "Sign in",
                         detail: ts.status.map { $0.loggedIn ? "Signed in as \($0.dnsName ?? "this Mac")." : "Open Tailscale from the menu bar and sign in." }
                            ?? (ts.cli == nil ? "After installing, sign in from the Tailscale menu bar icon." : "Tailscale isn’t running. Open it and sign in."))
                CheckRow(ok: ts.status?.httpsAvailable == true, title: "Turn on secure links (HTTPS)",
                         detail: ts.status?.httpsAvailable == true ? "Ready." : "In Tailscale’s admin console, open DNS and turn on MagicDNS and HTTPS Certificates.")
            }
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 12, style: .continuous).fill(.quaternary.opacity(0.4)))
            HStack(spacing: 10) {
                if ts.cli == nil {
                    Button("Download Tailscale") { NSWorkspace.shared.open(URL(string: "https://tailscale.com/download/mac")!) }
                        .buttonStyle(.smallPill)
                } else if ts.status?.loggedIn != true {
                    Button("Open Tailscale") { openTailscale() }.buttonStyle(.smallPill)
                }
                if ts.status?.loggedIn == true, ts.status?.httpsAvailable != true {
                    Button("Open Admin Console") { NSWorkspace.shared.open(URL(string: "https://login.tailscale.com/admin/dns")!) }
                        .buttonStyle(.smallPill)
                }
                Button("Check Again") { Task { await ts.refresh() } }
                    .buttonStyle(.smallQuietPill)
                    .disabled(ts.refreshing)
                if ts.refreshing { ProgressView().controlSize(.small) }
            }
            .padding(.top, 14)
            Text("This step only looks at Tailscale. Nothing changes until you choose how guests join.")
                .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true).padding(.top, 14)
        }
        .task { await ts.refresh() }
    }

    private func openTailscale() {
        let candidates = ["/Applications/Tailscale.app", FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Applications/Tailscale.app").path]
        if let path = candidates.first(where: { FileManager.default.fileExists(atPath: $0) }) {
            NSWorkspace.shared.openApplication(at: URL(fileURLWithPath: path), configuration: NSWorkspace.OpenConfiguration())
        } else {
            NSWorkspace.shared.open(URL(string: "https://tailscale.com/download/mac")!)
        }
    }
}

private struct GuestAccessStep: View {
    @ObservedObject var model: AppModel
    @State private var pending: TailscaleSetup.Plan?

    var body: some View {
        let c = model.onboardingContext
        VStack(alignment: .leading, spacing: 12) {
            StepHeader(title: "How will guests join?",
                       subtitle: "Choose who can open your meeting links. You can change this any time in Settings.")
            if model.tailscale.pendingApproval != nil {
                PendingApprovalBanner(model: model)
            }
            ChoiceCard(symbol: "globe", title: "Let guests join from anywhere",
                       detail: "Anyone with the link can ask to join from their browser, no Tailscale needed. You let each guest in.",
                       badge: "Recommended", availability: OnboardingFlow.availability(of: .publicLink, c)) {
                pending = model.tailscale.plan(.enablePublic)
            }
            ChoiceCard(symbol: "lock.shield", title: "Only guests on my Tailscale network",
                       detail: "Only people on your Tailscale network, or people you’ve shared this Mac with, can open the link.",
                       badge: nil, availability: OnboardingFlow.availability(of: .privateTailnet, c)) {
                pending = model.tailscale.plan(.enablePrivate)
            }
            Text(c.guestAccess == .off
                 ? "Not sure yet? Click Decide Later. p2p.video works on this Mac without guest links."
                 : "Guests can join. Continue, or choose the other option to switch.")
                .font(.callout).foregroundStyle(.secondary).padding(.top, 4)
        }
        .sheet(item: $pending) { plan in
            ConfirmPlanSheet(plan: plan, model: model) { pending = nil }
        }
    }
}

private struct ChoiceCard: View {
    let symbol: String
    let title: String
    let detail: String
    let badge: String?
    let availability: ChoiceAvailability
    let action: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 14) {
            Image(systemName: symbol).font(.title2).foregroundStyle(Theme.accent).frame(width: 30)
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 8) {
                    Text(title).font(.headline)
                    if let badge {
                        Text(badge).font(.caption2.weight(.semibold)).foregroundStyle(Theme.accent)
                            .padding(.horizontal, 7).padding(.vertical, 2)
                            .background(Capsule().fill(Theme.accentSoft))
                    }
                }
                Text(detail).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                switch availability {
                case .unavailable(let why), .availableWithCaveat(let why):
                    Label(why, systemImage: "info.circle").font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        .padding(.top, 2)
                default: EmptyView()
                }
            }
            Spacer(minLength: 8)
            if availability == .current {
                Label("On", systemImage: "checkmark.circle.fill").foregroundStyle(.green).font(.callout.weight(.semibold))
            } else {
                Button("Choose", action: action)
                    .buttonStyle(.smallQuietPill)
                    .disabled(!availability.selectable)
            }
        }
        .padding(16)
        .background(RoundedRectangle(cornerRadius: 14, style: .continuous).fill(Theme.surface))
        .overlay(RoundedRectangle(cornerRadius: 14, style: .continuous)
            .strokeBorder(availability == .current ? Theme.accent : Color.secondary.opacity(0.22), lineWidth: availability == .current ? 1.5 : 1))
    }
}

private struct BrowserStep: View {
    @ObservedObject var model: AppModel

    var body: some View {
        let chrome = model.chromeURL != nil
        VStack(alignment: .leading, spacing: 14) {
            StepHeader(title: "Choose your browser",
                       subtitle: "p2p.video opens in a browser window. Google Chrome is recommended: it’s what p2p.video is tested with for camera, microphone and recording.")
            HStack(spacing: 10) {
                Image(systemName: chrome ? "checkmark.circle.fill" : "exclamationmark.circle.fill")
                    .foregroundStyle(chrome ? .green : .orange)
                Text(chrome ? "Google Chrome is installed." : "Google Chrome isn’t installed.")
                if !chrome {
                    Button("Get Chrome") { NSWorkspace.shared.open(BrowserDetection.chromeDownload) }.buttonStyle(.smallPill)
                    Button("Check Again") { model.refreshBrowsers() }.buttonStyle(.smallQuietPill)
                }
            }
            .font(.callout)
            Picker("Open p2p.video in", selection: Binding(
                get: { model.effectiveBrowser },
                set: { model.settings.dashboardBrowser = $0 })) {
                Text("Google Chrome (recommended)").tag(DashboardBrowser.chrome)
                Text("Default browser").tag(DashboardBrowser.systemDefault)
            }
            .pickerStyle(.radioGroup)
            .disabled(!chrome)
            Label {
                Text("The first time you join a meeting, the browser asks for your camera and microphone. Click **Allow**. No preview? Open System Settings → Privacy & Security → Camera (and Microphone) and turn on \(model.effectiveBrowser == .chrome ? "Google Chrome" : "your browser").")
                    .fixedSize(horizontal: false, vertical: true)
            } icon: { Image(systemName: "video.badge.checkmark").foregroundStyle(Theme.accent) }
            .font(.callout)
            .padding(14)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 12, style: .continuous).fill(.quaternary.opacity(0.4)))
        }
        .onAppear {
            model.refreshBrowsers()
            if model.settings.dashboardBrowser == nil { model.settings.dashboardBrowser = model.effectiveBrowser }
        }
    }
}

private struct FirstRoomStep: View {
    @ObservedObject var model: AppModel
    @State private var name = "The Podcast"
    @State private var working = false
    @State private var created: RoomLink?
    @State private var error: String?
    @State private var copied = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            StepHeader(title: "Create your first room",
                       subtitle: "A room is a meeting place with a link that never changes, so guests can bookmark it. Great for a regular show or team meeting.")
            if let created {
                VStack(alignment: .leading, spacing: 10) {
                    Label("“\(created.name)” is ready.", systemImage: "checkmark.circle.fill").foregroundStyle(.green).font(.headline)
                    Text(created.guestUrlRemote
                         ? "Send guests its link. It works for every meeting in this room."
                         : "Its link works on this Mac for now. Choose how guests join (previous step or Settings) so guests can open it.")
                        .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    if let url = created.guestUrl {
                        Button {
                            model.copyToClipboard(url); copied = true
                        } label: {
                            Label(copied ? "Copied" : "Copy Room Link", systemImage: copied ? "checkmark" : "link")
                        }
                        .buttonStyle(.smallPill)
                    }
                }
                .padding(16).frame(maxWidth: .infinity, alignment: .leading)
                .background(RoundedRectangle(cornerRadius: 12, style: .continuous).fill(.quaternary.opacity(0.4)))
            } else {
                HStack {
                    TextField("Room name", text: $name).textFieldStyle(.roundedBorder).frame(maxWidth: 320)
                        .onSubmit { create() }
                    Button("Create Room") { create() }
                        .buttonStyle(.smallPill)
                        .disabled(working || name.trimmingCharacters(in: .whitespaces).isEmpty || !model.supervisor.isHealthy)
                    if working { ProgressView().controlSize(.small) }
                }
                if !model.supervisor.isHealthy {
                    Text("p2p.video isn’t ready yet: \(model.problem ?? model.statusLine)").font(.caption).foregroundStyle(.orange)
                }
            }
            if let error { Label(error, systemImage: "xmark.octagon.fill").foregroundStyle(.red).font(.callout) }
            if !model.rooms.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    Text("Your rooms").font(.caption.weight(.semibold)).foregroundStyle(.secondary)
                    ForEach(model.rooms, id: \.id) { r in Label(r.name, systemImage: "door.left.hand.open").font(.callout) }
                }
                .padding(.top, 6)
            }
        }
    }

    private func create() {
        guard !working else { return }
        working = true
        error = nil
        Task {
            switch await model.createRoom(named: name) {
            case .success(let link): created = link
            case .failure(let e): error = e.description
            }
            working = false
        }
    }
}

private struct DoneStep: View {
    @ObservedObject var model: AppModel

    var body: some View {
        let c = model.onboardingContext
        VStack(alignment: .leading, spacing: 14) {
            StepHeader(title: "You’re all set",
                       subtitle: "Start a meeting from the menu bar or the p2p.video window whenever you’re ready.")
            VStack(alignment: .leading, spacing: 12) {
                CheckRow(ok: c.daemonHealthy, title: "p2p.video", detail: model.statusLine)
                CheckRow(ok: c.tailscaleReady, optional: !c.tailscaleReady, title: "Tailscale",
                         detail: c.tailscaleReady ? "Connected." : model.tailscale.summary)
                CheckRow(ok: c.guestAccess != .off, optional: c.guestAccess == .off, title: "Guests", detail: guestText(c.guestAccess))
                CheckRow(ok: true, title: "Browser", detail: model.effectiveBrowser.label)
                CheckRow(ok: c.roomCount > 0, optional: c.roomCount == 0, title: "Rooms",
                         detail: c.roomCount == 0 ? "None yet. Create one from p2p.video’s home." : model.rooms.map(\.name).joined(separator: ", "))
            }
            .padding(16).frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 12, style: .continuous).fill(.quaternary.opacity(0.4)))
            Text("Before an important recording, run a short test call to check your camera and microphone. You can reopen this guide from Settings → General.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }

    private func guestText(_ m: GuestAccessMode) -> String {
        switch m {
        case .off: return "Only this Mac for now."
        case .privateTailnet: return "Guests on your Tailscale network can join."
        case .publicFunnel: return "Guests can join from anywhere."
        }
    }
}

// MARK: icon

/// The app icon, drawn from the same code that builds AppIcon.icns.
struct AppIconView: View {
    let size: CGFloat
    @MainActor private static var cache: [Int: CGImage] = [:]

    var body: some View {
        let px = Int(size * 2)
        let image = Self.cache[px] ?? {
            let img = AppIconArtwork.image(pixels: px)
            Self.cache[px] = img
            return img
        }()
        if let image {
            Image(decorative: image, scale: 2).resizable().interpolation(.high).frame(width: size, height: size)
        } else {
            Image(systemName: "video.fill").frame(width: size, height: size)
        }
    }
}
