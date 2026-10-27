import AppKit
import Combine
import ServiceManagement
import StudioLinkCore
import StudioLinkSpeech
import SwiftUI

/// App-wide state and actions shared by the menu bar, control center and settings.
@MainActor
final class AppModel: ObservableObject {
    let configuration: HostConfiguration
    let supervisor: DaemonSupervisor
    let tailscale: TailscaleSetup
    let notifier = Notifier()
    let readiness = ReadinessMonitor()
    let updater = AppUpdater()
    /// Upcoming meetings, reminders and calendar write-back [SL-P6-1].
    let calendar: CalendarMonitor
    /// Google Chrome's app bundle, if installed (re-detected on demand).
    @Published private(set) var chromeURL: URL?
    /// The guide opens automatically once per launch until it is completed.
    private var onboardingAutoOpened = false

    @Published var settings: DesktopSettings {
        didSet {
            persistSettings()
            if settings.captionProvider != oldValue.captionProvider { updateCaptionProvider() }
            if settings.transcriptionProvider != oldValue.transcriptionProvider { updateTranscriptionProvider() }
            if settings.guestAccess != oldValue.guestAccess || settings.publicURL != oldValue.publicURL {
                Task { await refreshReadiness() }
            }
        }
    }
    /// Live-captions provider connection, safe to show (no tokens or speech).
    @Published private(set) var captionStatus = "Live captions are off."
    private var captionProvider: CaptionProviderClient?
    /// Post-take transcription provider status, safe to show [SL-P4-3].
    @Published private(set) var transcriptionStatus = "Built-in transcription is off."
    /// True when the recognizer needs Speech Recognition permission (only the
    /// pre-macOS 26 engine does); Settings then offers an Allow button.
    @Published private(set) var transcriptionNeedsPermission = false
    private var transcriptionProvider: TranscriptionProviderClient?
    @Published var banner: String?
    @Published private(set) var loginItemEnabled = false
    @Published private(set) var loginItemNote: String?
    /// The in-app studio window [SL-P2-3] (one window, one meeting at a time).
    private(set) var studioWindow: StudioWindowController!

    private var planner = NotificationPlanner()
    private var cancellables: Set<AnyCancellable> = []

    init(configuration: HostConfiguration) {
        self.configuration = configuration
        let loaded = DesktopSettings.load(from: configuration.settingsFile, legacyPublicURL: configuration.legacyPublicURLFile)
        self.settings = loaded
        let supervisor = DaemonSupervisor(configuration: configuration) { loaded }
        self.supervisor = supervisor
        self.tailscale = TailscaleSetup(configuration: configuration)
        self.calendar = CalendarMonitor(ledgerFile: configuration.dataDirectory.appendingPathComponent("calendar-sync.json"))
        supervisor.settingsProvider = { [weak self] in self?.settings ?? loaded }
        supervisor.onState = { [weak self] state in self?.handle(state) }
        // Re-publish child changes so SwiftUI views observing the model update.
        supervisor.objectWillChange.sink { [weak self] _ in self?.objectWillChange.send() }.store(in: &cancellables)
        tailscale.objectWillChange.sink { [weak self] _ in self?.objectWillChange.send() }.store(in: &cancellables)
        readiness.objectWillChange.sink { [weak self] _ in self?.objectWillChange.send() }.store(in: &cancellables)
        calendar.objectWillChange.sink { [weak self] _ in self?.objectWillChange.send() }.store(in: &cancellables)
        refreshLoginItem()
        refreshBrowsers()
        studioWindow = StudioWindowController(model: self)
    }

    // MARK: lifecycle

    func launch() async {
        await supervisor.start()
        updateCaptionProvider()
        updateTranscriptionProvider()
        await tailscale.refresh()
        await refreshReadiness()
        startReadinessChecks()
        notifier.onStartMeeting = { [weak self] roomId in self?.openRoom(id: roomId) }
        calendar.start { [weak self] in await self?.calendarTick() }
        // First run: the setup guide ends by opening the dashboard instead.
        if configuration.openBrowserOnLaunch, supervisor.isHealthy, !settings.studioInApp, settings.onboardingCompleted {
            // Launcher parity: the dashboard opens on launch.
            openDashboard()
        }
    }

    // MARK: setup guide [SL-P2-2]

    /// True once per launch while the guide has not been completed.
    func consumeOnboardingAutoOpen() -> Bool {
        guard !settings.onboardingCompleted, !onboardingAutoOpened else { return false }
        onboardingAutoOpened = true
        return true
    }

    func completeOnboarding(openDashboard open: Bool) {
        settings.onboardingCompleted = true
        if open, supervisor.isHealthy { openDashboard() }
    }

    var onboardingContext: OnboardingContext {
        let s = tailscale.status
        return OnboardingContext(
            daemonHealthy: supervisor.isHealthy,
            tailscaleInstalled: tailscale.cli != nil,
            tailscaleLoggedIn: s?.loggedIn == true,
            httpsAvailable: s?.httpsAvailable == true,
            funnelAllowed: s?.funnelReadyForPublicGuests == true,
            guestAccess: tailscale.observation?.mode ?? settings.guestAccess,
            chromeInstalled: chromeURL != nil,
            roomCount: rooms.count)
    }

    /// Creates a persistent room through the loopback management API.
    func createRoom(named name: String) async -> Result<RoomLink, HostClientError> {
        guard let token = HostClient.readDesktopToken(configuration.desktopTokenFile) else { return .failure(.missingDesktopToken) }
        do {
            return .success(try await supervisor.client.createRoom(name: name, desktopToken: token))
        } catch let e as HostClientError {
            return .failure(e)
        } catch {
            return .failure(.http(0, "Could not reach the host: \(error.localizedDescription)"))
        }
    }

    func copyToClipboard(_ text: String) {
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString(text, forType: .string)
    }

    // MARK: browser [SL-P2-2]

    func refreshBrowsers() {
        chromeURL = BrowserDetection.locateChrome(home: FileManager.default.homeDirectoryForCurrentUser) {
            FileManager.default.fileExists(atPath: $0.path)
        }
    }

    var effectiveBrowser: DashboardBrowser {
        BrowserDetection.effective(preference: settings.dashboardBrowser, chromeInstalled: chromeURL != nil)
    }

    /// Opens a dashboard page in the chosen browser (Chrome or the default).
    func openInBrowser(_ url: URL) {
        switch BrowserDetection.target(preference: settings.dashboardBrowser, chrome: chromeURL) {
        case .application(let app):
            NSWorkspace.shared.open([url], withApplicationAt: app, configuration: NSWorkspace.OpenConfiguration()) { _, error in
                guard error != nil else { return }
                DispatchQueue.main.async { _ = NSWorkspace.shared.open(url) }
            }
        case .systemDefault:
            NSWorkspace.shared.open(url)
        }
    }

    // MARK: podcast-day checklist [SL-P2-2]

    private func startReadinessChecks() {
        readiness.start { [weak self] in
            guard let self else { return }
            await self.refreshReadiness()
        }
    }

    func refreshReadiness() async {
        await readiness.refresh(dataDirectory: configuration.dataDirectory, guestAccess: settings.guestAccess, publicURL: settings.publicURL)
    }

    var tailscaleReadiness: TailscaleReadiness {
        guard tailscale.cli != nil else { return tailscale.refreshing ? .checking : .notInstalled }
        guard let s = tailscale.status else { return tailscale.lastError == nil ? .checking : .notRunning }
        if s.needsLogin { return .needsLogin }
        return s.loggedIn ? .connected(dnsName: s.dnsName) : .notRunning
    }

    var checklist: [ReadinessItem] {
        PodcastChecklist.evaluate(ReadinessInputs(
            daemonHealthy: supervisor.isHealthy, daemonDetail: problem ?? statusLine, tailscale: tailscaleReadiness,
            guestAccess: settings.guestAccess, publicURL: settings.publicURL, linkCheck: readiness.linkCheck,
            freeBytes: readiness.freeBytes, power: readiness.power, browser: effectiveBrowser))
    }

    /// Opens the call-first home, where the device check is the test call.
    func runTestCall() async { await startMeeting() }

    /// Runs the on-device caption recognizer as the daemon's caption provider
    /// [SL-P4-2]. It reconnects on its own across daemon restarts.
    private func updateCaptionProvider() {
        guard settings.captionProvider else {
            captionProvider?.stop()
            captionProvider = nil
            captionStatus = "Live captions are off."
            return
        }
        guard captionProvider == nil else { return }
        let tokenFile = configuration.desktopTokenFile
        let client = CaptionProviderClient(baseURL: supervisor.client.baseURL, factory: AppleSpeechRecognizerFactory()) {
            HostClient.readDesktopToken(tokenFile)
        }
        captionProvider = client
        client.start { [weak self] status in
            Task { @MainActor in self?.captionStatus = Self.describe(status) }
        }
    }

    nonisolated static func describe(_ status: CaptionProviderClient.Status) -> String {
        switch status {
        case .stopped: return "Live captions are off."
        case .connecting: return "Connecting to the host…"
        case .connected: return "Ready: the host can turn on captions in any meeting."
        case .waiting(let reason): return reason
        }
    }

    /// Runs the on-device file recognizer as the daemon's transcription
    /// provider, the "Built-in" engine for post-take transcripts [SL-P4-3].
    /// Independent of live captions; reconnects across daemon restarts.
    private func updateTranscriptionProvider() {
        guard settings.transcriptionProvider else {
            transcriptionProvider?.stop()
            transcriptionProvider = nil
            transcriptionStatus = "Built-in transcription is off. Transcripts need Minutes instead."
            transcriptionNeedsPermission = false
            return
        }
        guard transcriptionProvider == nil else { return }
        let tokenFile = configuration.desktopTokenFile
        let client = TranscriptionProviderClient(baseURL: supervisor.client.baseURL, transcriber: AppleFileTranscriber()) {
            HostClient.readDesktopToken(tokenFile)
        }
        transcriptionProvider = client
        client.start { [weak self] status in
            Task { @MainActor in
                self?.transcriptionStatus = Self.describe(status)
                if case .connected(let availability) = status {
                    self?.transcriptionNeedsPermission = availability.state == "not_authorized"
                }
            }
        }
    }

    /// Asks for Speech Recognition (needed only by the pre-macOS 26 engine),
    /// then re-announces availability to the host.
    func allowSpeechRecognition() async {
        _ = await AppleFileTranscriber.requestAuthorization()
        await transcriptionProvider?.refreshAvailability()
    }

    nonisolated static func describe(_ status: TranscriptionProviderClient.Status) -> String {
        switch status {
        case .stopped: return "Built-in transcription is off."
        case .connecting: return "Connecting to the host…"
        case .connected(.ready): return "Ready: Transcribe in the export panel uses this Mac's on-device speech recognition."
        case .connected(let availability): return availability.message ?? "On-device transcription is unavailable on this Mac."
        case .transcribing: return "Transcribing a recording for the host…"
        case .waiting(let reason): return reason
        }
    }

    private func persistSettings() {
        do { try settings.save(to: configuration.settingsFile) } catch {
            banner = "Could not save settings: \(error.localizedDescription)"
        }
    }

    // MARK: status for the UI

    var hostState: HostState? { supervisor.hostState }
    var activity: DaemonActivity { supervisor.hostState?.activity ?? .idle }

    var statusLine: String {
        switch supervisor.phase {
        case .idle, .starting: return "Starting p2p.video…"
        case .running(let adopted): return adopted ? "Ready for meetings (already running)" : "Ready for meetings"
        case .restarting(let after): return after > 0 ? "Restarting in \(Int(after)) s…" : "Restarting…"
        case .blocked: return "Another app is using port \(configuration.port)"
        case .failed: return "p2p.video stopped"
        case .stopped: return "p2p.video stopped"
        }
    }

    /// One line of what is happening right now, for the menu bar and header.
    var activityLine: String? {
        guard let s = hostState else { return nil }
        if s.recording { return "Recording" }
        if s.live { return "Live" }
        if s.waitingCount > 0 { return "\(s.waitingCount) waiting to join" }
        if !s.activeCalls.isEmpty {
            let people = s.activeCalls.reduce(0) { $0 + $1.admitted }
            return s.activeCalls.count == 1 ? "In a meeting (\(people) \(people == 1 ? "person" : "people"))" : "\(s.activeCalls.count) meetings in progress"
        }
        return nil
    }

    var problem: String? {
        switch supervisor.phase {
        case .blocked(let m), .failed(let m): return m
        default: return supervisor.healthWarning
        }
    }

    var menuBarSymbol: String {
        if hostState?.recording == true { return "record.circle.fill" }
        if hostState?.live == true { return "dot.radiowaves.left.and.right" }
        if (hostState?.waitingCount ?? 0) > 0 { return "person.crop.circle.badge.clock" }
        if !supervisor.isHealthy { return "video.slash" }
        return "video"
    }

    /// Persistent rooms from the last host state poll [SL-P1-5].
    var rooms: [HostState.Room] { hostState?.roomList ?? [] }

    // MARK: actions

    /// Opens the call-first home: device check, then New meeting or a room.
    func startMeeting() async {
        guard supervisor.isHealthy else { banner = problem ?? "The host is not running yet."; return }
        openStudio(DashboardURLs.home(configuration.baseURL))
    }

    /// Opens the home with a room preselected.
    func openRoom(_ room: HostState.Room) {
        guard supervisor.isHealthy else { banner = problem ?? "The host is not running yet."; return }
        openStudio(DashboardURLs.room(configuration.baseURL, id: room.id))
    }

    /// Opens a room by ID (meeting reminder's "Start meeting").
    func openRoom(id: String) {
        openRoom(rooms.first { $0.id == id } ?? HostState.Room(id: id, name: "", live: false))
    }

    // MARK: calendar [SL-P6-1]

    func calendarTick() async {
        guard supervisor.isHealthy else { return }
        let due = await calendar.refresh(settings: settings, client: supervisor.client,
                                         desktopToken: HostClient.readDesktopToken(configuration.desktopTokenFile))
        for r in due { notifier.post(r) }
    }

    /// The Settings toggle: asks macOS for access only here, on the user's click.
    func setCalendarEnabled(_ on: Bool) async {
        if on, calendar.authorization == .notDetermined { _ = await calendar.requestAccess() }
        settings.calendarEnabled = on
        await calendarTick()
    }

    func setCalendarWrite(_ on: Bool) async {
        if on, calendar.authorization == .notDetermined { _ = await calendar.requestAccess() }
        calendar.refreshAuthorization()
        if on, settings.calendarWriteCalendarID == nil || !calendar.calendars.contains(where: { $0.id == settings.calendarWriteCalendarID }) {
            settings.calendarWriteCalendarID = calendar.calendars.first?.id
        }
        settings.calendarWriteEnabled = on && calendar.authorization == .fullAccess
        await calendarTick()
    }

    /// Copies a room's stable guest link (fetched on demand; never stored).
    func copyRoomLink(_ room: HostState.Room) async {
        do {
            guard let token = HostClient.readDesktopToken(configuration.desktopTokenFile) else {
                throw HostClientError.missingDesktopToken
            }
            let link = try await supervisor.client.roomLink(id: room.id, desktopToken: token)
            guard let url = link.guestUrl else {
                banner = "\(room.name) has no guest link. Create one from the home screen."
                return
            }
            NSPasteboard.general.clearContents()
            NSPasteboard.general.setString(url, forType: .string)
            banner = link.guestUrlRemote
                ? "Guest link for \(room.name) copied. It works for every meeting in that room."
                : "Guest link for \(room.name) copied. Set up guest access in Settings so it works beyond this Mac."
        } catch {
            banner = "Could not read the room’s guest link: \(error)"
        }
    }

    func openDashboard() {
        openInBrowser(signedIn(configuration.baseURL))
    }

    /// Every dashboard page the app opens carries the daemon's current launch
    /// token, which signs that browser (or the in-app web view) in as the
    /// host [SL-SEC-1]. Read fresh each time: a daemon restart replaces it.
    private func signedIn(_ url: URL) -> URL {
        DashboardURLs.launch(url, token: HostClient.readLaunchToken(configuration.launchTokenFile))
    }

    private func openStudio(_ url: URL) {
        let url = signedIn(url)
        if settings.studioInApp {
            studioWindow.open(url)
        } else {
            openInBrowser(url)
        }
    }

    func revealData() { NSWorkspace.shared.activateFileViewerSelecting([configuration.dataDirectory]) }
    func revealLog() { NSWorkspace.shared.activateFileViewerSelecting([configuration.daemonLog]) }

    enum ApplyOutcome: Equatable {
        case done
        /// Tailscale wants the owner to approve something first (SL-UX-2).
        case needsApproval(TailscaleApproval)
        case failed(String)
    }

    /// Applies a confirmed Tailscale plan and restarts the host if its flags change.
    func apply(_ plan: TailscaleSetup.Plan) async -> ApplyOutcome {
        switch await tailscale.run(plan) {
        case .success(let mode):
            let previous = settings
            settings.guestAccess = mode
            if mode != .off, let origin = tailscale.status?.httpsOrigin { settings.publicURL = origin }
            if (try? DaemonArguments.build(configuration: configuration, settings: previous)) != (try? DaemonArguments.build(configuration: configuration, settings: settings)) {
                await supervisor.restartForNewSettings()
            }
            return .done
        case .failure(.needsApproval(let approval)):
            return .needsApproval(approval)
        case .failure(.message(let m)):
            return .failed(m)
        }
    }

    /// "Try again" after approving in Tailscale: re-runs the same confirmed plan.
    func retryAfterApproval() async -> ApplyOutcome? {
        guard let pending = tailscale.pendingApproval, let plan = tailscale.plan(pending.kind) else { return nil }
        return await apply(plan)
    }

    func setPublicSTUN(_ on: Bool) async {
        settings.publicSTUN = on
        await supervisor.restartForNewSettings()
    }

    // MARK: login item

    func refreshLoginItem() {
        guard Bundle.main.bundleIdentifier != nil else {
            loginItemNote = "Available when p2p.video runs from its app bundle."
            return
        }
        let status = SMAppService.mainApp.status
        loginItemEnabled = status == .enabled
        loginItemNote = status == .requiresApproval ? "Approve p2p.video in System Settings → General → Login Items." : nil
    }

    func setLoginItem(_ on: Bool) {
        guard Bundle.main.bundleIdentifier != nil else { return }
        do {
            if on { try SMAppService.mainApp.register() } else { try SMAppService.mainApp.unregister() }
        } catch {
            loginItemNote = "Could not change the login item: \(error.localizedDescription)"
        }
        refreshLoginItem()
    }

    // MARK: notifications

    private func handle(_ state: HostState) {
        let events = planner.plan(state, guests: settings.notifyGuestWaiting, takes: settings.notifyTakeProgress)
        for e in events { notifier.post(e) }
    }
}
