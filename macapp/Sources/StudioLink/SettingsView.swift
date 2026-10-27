import AppKit
import StudioLinkCore
import SwiftUI

struct SettingsView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        TabView {
            GuestAccessPane(model: model)
                .tabItem { Label("Guest Access", systemImage: "person.2.wave.2") }
            GeneralPane(model: model)
                .tabItem { Label("General", systemImage: "gearshape") }
            CalendarPane(model: model, calendar: model.calendar)
                .tabItem { Label("Calendar", systemImage: "calendar") }
            AdvancedPane(model: model)
                .tabItem { Label("Advanced", systemImage: "wrench.and.screwdriver") }
        }
        .frame(width: 640, height: 600)
        .tint(Theme.accent)
    }
}

// MARK: General

struct GeneralPane: View {
    @ObservedObject var model: AppModel
    @ObservedObject var updater: AppUpdater
    @Environment(\.openWindow) private var openWindow

    init(model: AppModel) {
        self.model = model
        self.updater = model.updater
    }

    var body: some View {
        Form {
            Section {
                Toggle("Open p2p.video at login", isOn: Binding(get: { model.loginItemEnabled }, set: { model.setLoginItem($0) }))
                if let note = model.loginItemNote { Text(note).font(.caption).foregroundStyle(.secondary) }
                LabeledContent("Setup guide") {
                    Button("Show Setup Guide…") {
                        openWindow(id: "onboarding")
                        NSApp.activate(ignoringOtherApps: true)
                    }
                }
            }
            Section("About") {
                LabeledContent("Version", value: ProductInfo.versionLine(
                    shortVersion: Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String,
                    bundleVersion: Bundle.main.object(forInfoDictionaryKey: "CFBundleVersion") as? String,
                    buildID: model.configuration.expectedBuildID))
                LabeledContent("License", value: "Free software, \(ProductInfo.license)")
                LabeledContent("Source code") {
                    Link(ProductInfo.sourceURL.absoluteString, destination: ProductInfo.sourceURL)
                }
            }
            Section {
                Picker("Open meetings in", selection: $model.settings.studioInApp) {
                    Text("A p2p.video window").tag(true)
                    Text("Your browser").tag(false)
                }
                Picker("Browser", selection: Binding(get: { model.effectiveBrowser },
                                                     set: { model.settings.dashboardBrowser = $0 })) {
                    Text(model.chromeURL == nil ? "Google Chrome (not installed)" : "Google Chrome (recommended)").tag(DashboardBrowser.chrome)
                    Text("Default browser").tag(DashboardBrowser.systemDefault)
                }
                .disabled(model.chromeURL == nil)
                Text(model.settings.studioInApp
                     ? "Each meeting opens in its own p2p.video window. It asks before closing while recording and keeps the Mac awake."
                     : "Meetings open in the browser above. Chrome is the longest-tested choice.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                DetailsDisclosure {
                    Text("The p2p.video window keeps the call in this app: one window per meeting, it asks before closing during a recording, keeps the Mac awake while recording, and saves downloads to your Downloads folder. macOS asks once for camera, microphone and (for Present) screen recording access. Run the 2-minute test call in it first; the browser stays available (Chrome is the longest-tested path).")
                        .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
            } header: { Text("Meetings") }
            Section("Notifications") {
                Toggle("Someone is waiting to join", isOn: $model.settings.notifyGuestWaiting)
                Toggle("A recording finished uploading or exporting", isOn: $model.settings.notifyTakeProgress)
            }
            Section {
                Toggle("Live captions, recognized on this Mac", isOn: $model.settings.captionProvider)
                Text(model.captionStatus)
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                Text("When you turn on captions in a meeting, speech is turned into text right here. Audio and text never leave this Mac, and everyone sees that captions are on.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            } header: { Text("Captions") }
            Section {
                Toggle("Transcribe recordings on this Mac", isOn: $model.settings.transcriptionProvider)
                Text(model.transcriptionStatus)
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                if model.transcriptionNeedsPermission {
                    Button("Allow Speech Recognition…") { Task { await model.allowSpeechRecognition() } }
                }
                Text("After a recording is ready, Transcribe writes a transcript for each person using on-device speech recognition. Audio and text never leave this Mac. Choose “Built-in” when you get files for editing.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            } header: { Text("Transcripts") }
            Section {
                LabeledContent("Version", value: Self.versionText)
                HStack {
                    Text(updater.statusText).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    Spacer()
                    Button("Check for Updates…") { updater.checkForUpdates() }
                        .disabled(!updater.status.isEnabled || !updater.canCheckForUpdates)
                }
            } header: { Text("Updates") }
        }
        .formStyle(.grouped)
        .onAppear { model.refreshLoginItem(); model.refreshBrowsers() }
    }

    static var versionText: String {
        let info = Bundle.main.infoDictionary ?? [:]
        let short = info["CFBundleShortVersionString"] as? String ?? "development"
        let build = info["CFBundleVersion"] as? String
        return build.map { "\(short) (\($0))" } ?? short
    }
}

// MARK: Calendar [SL-P6-1]

struct CalendarPane: View {
    @ObservedObject var model: AppModel
    @ObservedObject var calendar: CalendarMonitor

    var body: some View {
        Form {
            Section {
                Text("p2p.video looks at the calendars already on this Mac (Google, iCloud or Exchange accounts in System Settings → Internet Accounts) for meetings that use one of your room links. Nothing is sent anywhere, and no Google sign-in is needed.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            Section("Upcoming meetings") {
                Toggle("Show upcoming meetings and remind me", isOn: Binding(get: { model.settings.calendarEnabled },
                                                                          set: { v in Task { await model.setCalendarEnabled(v) } }))
                Picker("Remind me", selection: $model.settings.calendarReminderMinutes) {
                    Text("Don’t remind").tag(0)
                    ForEach([1, 5, 10, 15, 30], id: \.self) { Text("\($0) min before").tag($0) }
                }
                .disabled(!model.settings.calendarEnabled)
                Text(accessLine).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                if !calendar.status.isEmpty {
                    Text(calendar.status).font(.caption).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true)
                }
            }
            Section("Add scheduled meetings to a calendar") {
                Toggle("Add meetings I schedule in p2p.video to", isOn: Binding(get: { model.settings.calendarWriteEnabled },
                                                                              set: { v in Task { await model.setCalendarWrite(v) } }))
                Picker("Calendar", selection: Binding(get: { model.settings.calendarWriteCalendarID ?? "" },
                                                      set: { model.settings.calendarWriteCalendarID = $0.isEmpty ? nil : $0; Task { await model.calendarTick() } })) {
                    if calendar.calendars.isEmpty { Text("No writable calendars").tag("") }
                    ForEach(calendar.calendars) { c in Text(c.account.isEmpty ? c.title : "\(c.title) (\(c.account))").tag(c.id) }
                }
                .disabled(!model.settings.calendarWriteEnabled || calendar.calendars.isEmpty)
                Text("Off by default. Changing a meeting updates its event, and cancelling removes it. The event holds the meeting link, so anyone who can see that calendar can ask to join.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            if !calendar.upcoming.isEmpty {
                Section("Next") {
                    ForEach(calendar.upcoming.prefix(5)) { m in
                        HStack {
                            VStack(alignment: .leading) {
                                Text(m.title)
                                Text("\(m.start.formatted(date: .abbreviated, time: .shortened)), \(m.roomName)").font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Button("Start") { model.openRoom(id: m.roomId) }
                        }
                    }
                }
            }
        }
        .formStyle(.grouped)
        .onAppear { calendar.refreshAuthorization() }
    }

    private var accessLine: String {
        model.settings.calendarEnabled || model.settings.calendarWriteEnabled
            ? calendar.authorization.explanation
            : "Off. Turning this on asks macOS for calendar access once. Meetings you schedule in p2p.video are shown either way."
    }
}

// MARK: Advanced

struct AdvancedPane: View {
    @ObservedObject var model: AppModel

    var body: some View {
        Form {
            Section("Network") {
                Toggle("Help guests connect directly (public STUN)", isOn: Binding(get: { model.settings.publicSTUN },
                                                                                 set: { v in Task { await model.setPublicSTUN(v) } }))
                Text("Uses a public STUN server so guests outside your network can connect directly. Changing it restarts p2p.video when nothing is recording.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            Section("This Mac") {
                LabeledContent("Address", value: "http://127.0.0.1:\(model.configuration.port)")
                LabeledContent("Relay (TURN)", value: model.configuration.turnListen)
                LabeledContent("Build", value: model.configuration.expectedBuildID)
                LabeledContent("Data folder") {
                    Text(model.configuration.dataDirectory.path).lineLimit(1).truncationMode(.middle).textSelection(.enabled)
                }
                HStack {
                    Button("Show Data Folder") { model.revealData() }
                    Button("Show Log") { model.revealLog() }
                        .help("Reveals daemon.log, the background service’s log, in Finder.")
                }
            }
        }
        .formStyle(.grouped)
    }
}

// MARK: Guest access (Tailscale guided setup)

struct GuestAccessPane: View {
    @ObservedObject var model: AppModel
    @State private var pending: TailscaleSetup.Plan?

    private var ts: TailscaleSetup { model.tailscale }

    var body: some View {
        Form {
            Section {
                HStack(alignment: .center, spacing: 14) {
                    Image(systemName: modeSymbol)
                        .font(.system(size: 26))
                        .foregroundStyle(Theme.accent)
                        .frame(width: 36)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(ts.summary).font(.headline)
                        Text(modeDetail).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
                .padding(.vertical, 4)
                if ts.pendingApproval != nil {
                    PendingApprovalBanner(model: model)
                }
            }
            Section {
                OptionRow(symbol: "person.2.wave.2", title: "Guests from anywhere",
                          detail: "Anyone with a meeting link can ask to join from their browser. You let each guest in.",
                          active: mode == .publicFunnel) {
                    if ts.observation?.anythingPublic == true {
                        actionButton(.disablePublic, label: "Turn Off…", enabled: !ts.running)
                    } else {
                        actionButton(.enablePublic, label: "Turn On…", prominent: true, enabled: ready)
                    }
                }
                OptionRow(symbol: "lock.shield", title: "Guests on your Tailscale network",
                          detail: "Only people on your Tailscale network, or people you’ve shared this Mac with, can open your links.",
                          active: mode == .privateTailnet) {
                    if ts.observation?.anythingPublic == true {
                        Text("Included").font(.callout).foregroundStyle(.secondary)
                    } else if ts.observation?.appRouted == true {
                        actionButton(.disablePrivate, label: "Turn Off…", enabled: !ts.running)
                    } else {
                        actionButton(.enablePrivate, label: "Turn On…", enabled: ready)
                    }
                }
            } header: {
                Text("Choose how guests join")
            } footer: {
                Text("p2p.video never changes Tailscale without your click. Your own controls always stay on this Mac.")
                    .font(.caption).foregroundStyle(.secondary)
            }
            Section("Tailscale") {
                CheckRow(ok: ts.cli != nil, title: "Installed", detail: ts.cli != nil ? "Found the Tailscale app." : "Install it from tailscale.com/download.")
                CheckRow(ok: ts.status?.loggedIn == true, title: "Signed in",
                         detail: ts.status.map { $0.loggedIn ? "Connected as \($0.dnsName ?? "this Mac")." : "Open Tailscale and sign in." } ?? (ts.lastError ?? "Not checked yet."))
                CheckRow(ok: ts.status?.magicDNSEnabled == true && ts.status?.httpsAvailable == true, title: "Secure links (HTTPS)",
                         detail: httpsDetail)
                CheckRow(ok: ts.status?.funnelReadyForPublicGuests == true, optional: true, title: "Public links allowed",
                         detail: ts.status?.funnelReadyForPublicGuests == true
                            ? "Ready for guests from anywhere."
                            : "Only needed for guests from anywhere. If Tailscale asks, p2p.video shows you where to approve it.")
                HStack {
                    Button("Check Again") { Task { await ts.refresh() } }.disabled(ts.refreshing)
                    if ts.refreshing { ProgressView().controlSize(.small) }
                    Spacer()
                    if ts.status?.loggedIn == true, ts.status?.httpsAvailable != true {
                        Button("Open Tailscale Settings") { NSWorkspace.shared.open(URL(string: "https://login.tailscale.com/admin/dns")!) }
                    }
                }
            }
            Section {
                DetailsDisclosure {
                    VStack(alignment: .leading, spacing: 6) {
                        LabeledContent("Current route") { Text(routeText).multilineTextAlignment(.trailing).textSelection(.enabled) }
                        if let path = ts.cli?.path { LabeledContent("Tailscale CLI") { Text(path).textSelection(.enabled) } }
                        if let suffix = ts.status?.magicDNSSuffix { LabeledContent("MagicDNS") { Text(suffix).textSelection(.enabled) } }
                        if !ts.transcript.isEmpty {
                            Text("Last change").font(.caption.weight(.medium)).padding(.top, 4)
                            ScrollView {
                                Text(ts.transcript.joined(separator: "\n"))
                                    .font(.caption.monospaced()).textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
                            }
                            .frame(maxHeight: 120)
                        }
                    }
                    .font(.caption)
                }
            }
        }
        .formStyle(.grouped)
        .task { await ts.refresh() }
        .sheet(item: $pending) { plan in
            ConfirmPlanSheet(plan: plan, model: model) { pending = nil }
        }
    }

    private var mode: GuestAccessMode { ts.observation?.mode ?? .off }

    private var ready: Bool { ts.status?.loggedIn == true && ts.status?.httpsAvailable == true && !ts.running }

    private var modeSymbol: String {
        switch ts.observation?.mode {
        case .publicFunnel?: return "globe"
        case .privateTailnet?: return "lock.shield"
        default: return "laptopcomputer"
        }
    }

    private var modeDetail: String {
        if let origin = ts.observation?.origin, mode != .off { return "Meeting links use \(origin)." }
        if ts.status?.loggedIn != true { return "Connect Tailscale on this Mac to let guests join from other devices." }
        return "Pick an option below to let guests join from their own devices."
    }

    private var httpsDetail: String {
        guard let s = ts.status else { return "Needs Tailscale signed in." }
        if s.magicDNSEnabled && s.httpsAvailable { return "Ready for \(s.dnsName ?? "this Mac")." }
        if !s.magicDNSEnabled { return "In Tailscale’s admin console, open DNS and turn on MagicDNS, then HTTPS Certificates." }
        return "In Tailscale’s admin console, open DNS and turn on HTTPS Certificates."
    }

    private var routeText: String {
        guard let o = ts.observation else { return "Unknown until Tailscale is connected." }
        switch o.mode {
        case .publicFunnel?: return "Public: \(o.origin ?? "") (Funnel :443 and TURN :8443)"
        case .privateTailnet?: return "Private: \(o.origin ?? "") (tailnet only)"
        case .off?: return o.conflictingAppTarget.map { ":443 serves another target (\($0))" } ?? "No Serve or Funnel route to p2p.video."
        case nil: return "Partial setup: app \(o.appRouted ? "routed" : "not routed")\(o.appFunneled ? " (public)" : ""), TURN \(o.turnRouted ? "routed" : "not routed")\(o.turnFunneled ? " (public)" : "")."
        }
    }

    @ViewBuilder
    private func actionButton(_ kind: TailscaleSetup.Plan.Kind, label: String, prominent: Bool = false, enabled: Bool) -> some View {
        if let plan = ts.plan(kind) {
            Button(label) { pending = plan }
                .buttonStyle(prominent ? .smallPill : .smallQuietPill)
                .disabled(!enabled)
                .accessibilityLabel(plan.title)
        }
    }
}

/// One guest-access option: icon, title, one line, and its button.
private struct OptionRow<Trailing: View>: View {
    let symbol: String
    let title: String
    let detail: String
    let active: Bool
    @ViewBuilder var trailing: Trailing

    var body: some View {
        HStack(alignment: .center, spacing: 12) {
            Image(systemName: symbol).font(.title3).foregroundStyle(Theme.accent).frame(width: 28)
            VStack(alignment: .leading, spacing: 2) {
                HStack(spacing: 6) {
                    Text(title).font(.body.weight(.medium))
                    if active {
                        Label("On", systemImage: "checkmark.circle.fill").labelStyle(.titleAndIcon)
                            .font(.caption.weight(.semibold)).foregroundStyle(.green)
                    }
                }
                Text(detail).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 8)
            trailing
        }
        .padding(.vertical, 2)
    }
}
