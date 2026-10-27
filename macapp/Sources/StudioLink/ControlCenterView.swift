import AppKit
import StudioLinkCore
import SwiftUI

/// The main window: a native control center for the host. The call itself
/// runs in the p2p.video studio window or the browser (Settings → General).
struct ControlCenterView: View {
    @ObservedObject var model: AppModel
    @Environment(\.openWindow) private var openWindow
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                header
                if let problem = model.problem {
                    Callout(symbol: "exclamationmark.triangle.fill", tint: .orange, text: problem)
                }
                if let reason = model.supervisor.pendingRestartReason {
                    Callout(symbol: "clock.arrow.circlepath", tint: .blue, text: reason)
                }
                if let banner = model.banner {
                    Callout(symbol: "info.circle.fill", tint: Theme.accent, text: banner) { model.banner = nil }
                }
                actions
                HStack(alignment: .top, spacing: 14) {
                    nowCard.frame(maxHeight: .infinity, alignment: .top)
                    guestAccessCard.frame(maxHeight: .infinity, alignment: .top)
                }
                .fixedSize(horizontal: false, vertical: true)
                checklistCard
                takesCard
                footer
            }
            .padding(24)
        }
        .frame(minWidth: 680, minHeight: 480)
        .background(Theme.canvas)
        .tint(Theme.accent)
        .onAppear {
            if model.consumeOnboardingAutoOpen() { openWindow(id: "onboarding") }
        }
    }

    // MARK: sections

    private var header: some View {
        HStack(spacing: 14) {
            AppIconView(size: 56)
                .padding(-4)
            VStack(alignment: .leading, spacing: 4) {
                Text("p2p.video").font(.title2.weight(.semibold))
                HStack(spacing: 14) {
                    HStack(spacing: 6) {
                        Circle().fill(statusColor).frame(width: 8, height: 8)
                        Text(model.activityLine ?? model.statusLine)
                    }
                    Label(model.tailscale.summary, systemImage: guestSymbol)
                }
                .font(.callout)
                .foregroundStyle(.secondary)
            }
            Spacer()
            Menu {
                Button("Restart p2p.video") { Task { await restartHost() } }
                Button("Show Log in Finder") { model.revealLog() }
                Button("Show Data Folder in Finder") { model.revealData() }
            } label: {
                Image(systemName: "ellipsis.circle")
            }
            .menuStyle(.borderlessButton)
            .fixedSize()
            .help("More")
        }
    }

    private var actions: some View {
        HStack(spacing: 10) {
            Button {
                Task { await model.startMeeting() }
            } label: {
                Label("Start a Meeting", systemImage: "video.badge.plus").frame(maxWidth: .infinity)
            }
            .buttonStyle(.pill)
            .keyboardShortcut("n")
            .disabled(!model.supervisor.isHealthy)
            Menu {
                ForEach(model.rooms, id: \.id) { room in
                    Button(room.name) { Task { await model.copyRoomLink(room) } }
                }
            } label: {
                Label("Copy Room Link", systemImage: "link").frame(maxWidth: .infinity)
            }
            .controlSize(.large)
            .disabled(model.rooms.isEmpty)
            .help(model.rooms.isEmpty ? "Create a room in p2p.video first." : "Copies a room’s link. It stays the same for every meeting.")
            Button {
                model.openDashboard()
            } label: {
                Label("Open Home", systemImage: "house").frame(maxWidth: .infinity)
            }
            .buttonStyle(.quietPill)
            .help("Opens p2p.video's home in \(model.effectiveBrowser.label): meetings, rooms and recordings.")
            .disabled(!model.supervisor.isHealthy)
        }
    }

    private var nowCard: some View {
        Card(title: "Now", symbol: "waveform") {
            if let state = model.hostState {
                VStack(alignment: .leading, spacing: 8) {
                    IndicatorRow(on: state.recording, onText: "Recording", offText: "Not recording", symbol: "record.circle", tint: .red)
                    IndicatorRow(on: state.live, onText: "Livestream is live", offText: "Not livestreaming", symbol: "dot.radiowaves.left.and.right", tint: .pink)
                    IndicatorRow(on: !state.activeCalls.isEmpty, onText: meetingsText(state),
                                 offText: "No meeting in progress", symbol: "person.2", tint: .green)
                    if state.waitingCount > 0 {
                        IndicatorRow(on: true, onText: "\(state.waitingCount) waiting to join", offText: "", symbol: "person.crop.circle.badge.clock", tint: .orange)
                    }
                    if state.uploadsPending > 0 {
                        IndicatorRow(on: true, onText: "Uploading \(state.uploadsPending) recording file\(state.uploadsPending == 1 ? "" : "s")", offText: "",
                                     symbol: "arrow.up.circle", tint: Theme.accent)
                    }
                    ForEach(state.sessions, id: \.id) { s in
                        VStack(alignment: .leading, spacing: 2) {
                            Text(s.title.isEmpty ? "Meeting" : s.title).font(.callout.weight(.medium))
                            Text(s.waiting.isEmpty ? "\(s.admitted) in the meeting" : "\(s.admitted) in the meeting. Waiting: \(s.waiting.map(\.name).joined(separator: ", "))")
                                .font(.caption).foregroundStyle(.secondary)
                        }
                        .padding(.top, 4)
                    }
                }
            } else if model.supervisor.isHealthy {
                Text(model.supervisor.lastStateError ?? "Checking what’s happening…").foregroundStyle(.secondary).font(.callout)
            } else {
                IndicatorRow(on: false, onText: "", offText: model.statusLine, symbol: "pause.circle", tint: .secondary)
            }
        }
    }

    private var guestAccessCard: some View {
        Card(title: "Guests", symbol: "person.2.wave.2") {
            VStack(alignment: .leading, spacing: 8) {
                Text(guestModeText).font(.callout.weight(.medium))
                Text(guestModeDetail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                if let origin = model.settings.publicURL, model.settings.guestAccess != .off {
                    Text(origin).font(.caption.monospaced()).foregroundStyle(.secondary).textSelection(.enabled)
                }
                if model.tailscale.pendingApproval != nil {
                    Label("Tailscale is waiting for your approval.", systemImage: "hand.raised.fill")
                        .font(.caption.weight(.medium)).foregroundStyle(.orange)
                }
                Button(model.settings.guestAccess == .off ? "Choose How Guests Join…" : "Change…") { openSettings() }
                    .buttonStyle(.smallQuietPill)
                    .padding(.top, 2)
            }
        }
    }

    /// Readiness before recording, refreshed every 30 s [SL-P2-2].
    private var checklistCard: some View {
        Card(title: "Before you record", symbol: "checklist") {
            VStack(alignment: .leading, spacing: 0) {
                let items = model.checklist
                ForEach(items) { item in
                    ReadinessRow(item: item)
                        .padding(.vertical, 6)
                    if item.id != items.last?.id { Divider() }
                }
                HStack(spacing: 10) {
                    Button {
                        Task { await model.runTestCall() }
                    } label: {
                        Label("Run a 2-Minute Test Call", systemImage: "play.circle")
                    }
                    .buttonStyle(.smallPill)
                    .disabled(!model.supervisor.isHealthy)
                    .help("Opens p2p.video’s device check. Start a meeting, join it from your phone with the room link, record a minute, then end it.")
                    Button("Check Again") {
                        Task {
                            await model.tailscale.refresh()
                            await model.refreshReadiness()
                        }
                    }
                    .buttonStyle(.smallQuietPill)
                    Spacer()
                    Text("Opens in \(model.settings.studioInApp ? "a p2p.video window" : model.effectiveBrowser.label)").font(.caption).foregroundStyle(.secondary)
                }
                .padding(.top, 10)
            }
        }
    }

    private var takesCard: some View {
        Card(title: "Recent recordings", symbol: "film.stack") {
            if let takes = model.hostState?.takes, !takes.isEmpty {
                VStack(spacing: 0) {
                    ForEach(takes, id: \.id) { t in
                        HStack {
                            Text("Recording \(t.number)").font(.callout.weight(.medium))
                            Text(t.sessionId).font(.caption).foregroundStyle(.tertiary).help("Meeting ID")
                            Spacer()
                            Text(takeStatus(t)).font(.caption).foregroundStyle(t.exported || t.uploadsComplete ? .green : .secondary)
                        }
                        .padding(.vertical, 5)
                        if t.id != takes.last?.id { Divider() }
                    }
                }
            } else {
                Text("Nothing recorded yet.").foregroundStyle(.secondary).font(.callout)
            }
        }
    }

    private var footer: some View {
        DetailsDisclosure {
            VStack(alignment: .leading, spacing: 3) {
                Text("Address: 127.0.0.1:\(String(model.configuration.port))")
                Text("Data folder: \(model.configuration.dataDirectory.path)").lineLimit(1).truncationMode(.middle)
                Text("Build: \(String(model.configuration.expectedBuildID.prefix(16)))")
            }
            .font(.caption.monospaced())
            .foregroundStyle(.secondary)
            .textSelection(.enabled)
        }
    }

    // MARK: helpers

    private var statusColor: Color {
        switch model.supervisor.phase {
        case .running: return model.hostState?.recording == true ? .red : .green
        case .starting, .restarting, .idle: return .yellow
        default: return .orange
        }
    }

    private var guestSymbol: String {
        switch model.tailscale.observation?.mode {
        case .publicFunnel?: return "globe"
        case .privateTailnet?: return "lock.shield"
        default: return "laptopcomputer"
        }
    }

    private var guestModeText: String {
        switch model.settings.guestAccess {
        case .off: return "Only this Mac"
        case .privateTailnet: return "Guests on your Tailscale network"
        case .publicFunnel: return "Guests from anywhere"
        }
    }

    private var guestModeDetail: String {
        switch model.settings.guestAccess {
        case .off: return "Guests can’t join from their own devices yet. Choose how guests join to share meeting links."
        case .privateTailnet: return "People on your Tailscale network, or people you’ve shared this Mac with, can join from a meeting link."
        case .publicFunnel: return "Anyone with a meeting link can ask to join from their browser. You let each guest in."
        }
    }

    private func meetingsText(_ state: HostState) -> String {
        let people = state.activeCalls.reduce(0) { $0 + $1.admitted }
        let who = "\(people) \(people == 1 ? "person" : "people")"
        return state.activeCalls.count == 1 ? "In a meeting with \(who)" : "\(state.activeCalls.count) meetings, \(who) in total"
    }

    private func takeStatus(_ t: HostState.Take) -> String {
        if t.status == "recording" { return "Recording now" }
        if t.exported { return "Files ready for editing" }
        if t.uploadsComplete { return "Saved on this Mac, ready for editing" }
        var text = "\(t.tracksUploaded) of \(t.tracks) files uploaded"
        if t.missingParticipants > 0 { text += ", waiting on \(t.missingParticipants) \(t.missingParticipants == 1 ? "person" : "people")" }
        return text
    }

    private func restartHost() async {
        if let warning = QuitWarning.message(model.hostState) {
            let alert = NSAlert()
            alert.alertStyle = .warning
            alert.messageText = "Restart p2p.video now?"
            alert.informativeText = "\(warning) Restarting disconnects everyone."
            alert.addButton(withTitle: "Cancel")
            alert.addButton(withTitle: "Restart Anyway")
            guard alert.runModal() == .alertSecondButtonReturn else { return }
        }
        await model.supervisor.restart()
    }
}

// MARK: building blocks

struct Card<Content: View>: View {
    let title: String
    let symbol: String
    @ViewBuilder var content: Content

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            Label {
                Text(title)
            } icon: {
                Image(systemName: symbol).foregroundStyle(Theme.accent)
            }
            .font(.headline)
            content
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(16)
        .background(RoundedRectangle(cornerRadius: 14, style: .continuous).fill(Theme.surface))
        .overlay(RoundedRectangle(cornerRadius: 14, style: .continuous).strokeBorder(.separator.opacity(0.6)))
    }
}

struct ReadinessRow: View {
    let item: ReadinessItem

    var body: some View {
        HStack(alignment: .firstTextBaseline, spacing: 10) {
            Image(systemName: symbol).foregroundStyle(tint).frame(width: 18)
            Text(item.title).font(.callout.weight(.medium)).frame(width: 150, alignment: .leading)
            Text(item.detail).font(.callout).foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
            Spacer(minLength: 0)
        }
        .accessibilityElement(children: .combine)
    }

    private var symbol: String {
        switch item.level {
        case .ok: return "checkmark.circle.fill"
        case .warning: return "exclamationmark.triangle.fill"
        case .problem: return "xmark.octagon.fill"
        case .neutral: return "info.circle"
        }
    }

    private var tint: Color {
        switch item.level {
        case .ok: return .green
        case .warning: return .orange
        case .problem: return .red
        case .neutral: return .secondary
        }
    }
}

struct IndicatorRow: View {
    let on: Bool
    let onText: String
    let offText: String
    let symbol: String
    let tint: Color

    var body: some View {
        HStack(spacing: 8) {
            Image(systemName: symbol)
                .foregroundStyle(on ? tint : Color.secondary)
                .frame(width: 20, alignment: .center)
            Text(on ? onText : offText).foregroundStyle(on ? .primary : .secondary)
        }
        .font(.callout)
    }
}

struct Callout: View {
    let symbol: String
    let tint: Color
    let text: String
    var dismiss: (() -> Void)?

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: symbol).foregroundStyle(tint)
            Text(text).fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
            Spacer(minLength: 0)
            if let dismiss {
                Button(action: dismiss) { Image(systemName: "xmark") }.buttonStyle(.borderless).help("Dismiss")
            }
        }
        .font(.callout)
        .padding(12)
        .background(RoundedRectangle(cornerRadius: 10, style: .continuous).fill(tint.opacity(0.12)))
    }
}
