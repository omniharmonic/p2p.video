import AppKit
import Combine
import StudioLinkCore
import WebKit

/// The in-app studio [SL-P2-3]: one AppKit window holding the host studio in a
/// WKWebView on the daemon's loopback origin. One window, one meeting at a
/// time; closing during a take asks first; App Nap and idle sleep are held off
/// while a call or take needs them; frame and full screen are remembered.
@MainActor
final class StudioWindowController: NSObject, NSWindowDelegate {
    struct Options {
        /// Downloads land here (default: the user's Downloads folder).
        var downloadsDirectory: URL?
        /// Records external opens instead of launching the browser (probe).
        var openExternally: ((URL) -> Void)?
        /// Answers the close confirmation without a dialog (probe).
        var autoAnswerClose: Bool?
        /// Probe-only private WebKit switches (mock capture, no occlusion).
        var probeMockCapture = false
        /// Probe-only script injected at document start (fetch hook).
        var probeUserScript: String?
    }

    private weak var model: AppModel?
    private let options: Options
    private let origin: DaemonOrigin
    private(set) var window: NSWindow?
    private(set) var webView: WKWebView?
    private var delegate: StudioWebDelegate?
    private var cancellables: Set<AnyCancellable> = []
    private var activityToken: NSObjectProtocol?
    private(set) var activity: StudioLifecycle.Activity = .none
    private(set) var inCall = false
    private var pageBadge = 0
    /// Bypasses the close confirmation once the user (or app quit) has decided.
    private var closeConfirmed = false
    /// For the probe: what the last close attempt decided.
    private(set) var lastCloseDecision: StudioLifecycle.CloseDecision?
    private(set) var pageMessages: [StudioPageMessage] = []
    private(set) var finishedDownloads: [URL] = []
    private(set) var failedDownloads: [String] = []
    /// Probe hook: the page finished a navigation.
    var onNavigationFinished: ((WKWebView) -> Void)?

    static let frameAutosaveName = "StudioLinkStudioWindow"

    init(model: AppModel, options: Options = Options()) {
        self.model = model
        self.options = options
        self.origin = DaemonOrigin(port: model.configuration.port)
        super.init()
        // @Published emits before the property changes: use the emitted value.
        model.supervisor.$hostState.receive(on: RunLoop.main).sink { [weak self] state in
            self?.refreshActivity(state: state)
        }.store(in: &cancellables)
    }

    var windowState: StudioLifecycle.WindowState {
        StudioLifecycle.WindowState(open: window != nil, inCall: inCall)
    }

    var isOpen: Bool { window != nil }

    // MARK: open

    /// Opens (or focuses) the studio at a signed-in URL. Never logs the URL.
    func open(_ url: URL) {
        switch StudioLifecycle.openDecision(current: webView?.url, requested: url, window: windowState) {
        case .focusExisting:
            focus()
        case .load:
            load(url)
        case .confirmReplace(let message):
            focus()
            let alert = NSAlert()
            alert.alertStyle = .warning
            alert.messageText = "Leave the current call?"
            alert.informativeText = message
            alert.addButton(withTitle: "Stay in Call")
            alert.addButton(withTitle: "Leave and Open")
            if let window {
                alert.beginSheetModal(for: window) { [weak self] response in
                    if response == .alertSecondButtonReturn { self?.load(url) }
                }
            } else if alert.runModal() == .alertSecondButtonReturn {
                load(url)
            }
        }
    }

    private func load(_ url: URL) {
        let view = webView ?? makeWebView()
        if window == nil { makeWindow(content: view) }
        view.load(URLRequest(url: url))
        focus()
    }

    func focus() {
        guard let window else { return }
        if options.autoAnswerClose != nil {
            // Probe: don't take focus (synthetic clicks activate briefly).
            window.orderFrontRegardless()
            return
        }
        NSApp.activate(ignoringOtherApps: true)
        window.makeKeyAndOrderFront(nil)
    }

    private func makeWebView() -> WKWebView {
        guard let model else { fatalError("studio window outlived the app model") }
        let delegate = StudioWebDelegate(port: model.configuration.port)
        if let open = options.openExternally { delegate.openExternally = open }
        if let dir = options.downloadsDirectory { delegate.downloadsDirectory = { dir } }
        delegate.onDownloadFinished = { [weak self] url in self?.downloadFinished(url) }
        delegate.onDownloadFailed = { [weak self] name, reason in
            self?.failedDownloads.append(name + ": " + reason)
            self?.model?.notifier.postDownloadFailed(name: name, reason: reason)
        }
        delegate.onPageMessage = { [weak self] message in self?.handle(message) }
        delegate.onNavigationFinished = { [weak self] view in self?.navigationFinished(view) }
        delegate.onContentProcessTerminated = { view in view.reload() }
        self.delegate = delegate
        let store = StudioWebViewFactory.dataStore(dataDirectory: model.configuration.webDataIdentityDirectory)
        let view = StudioWebViewFactory.make(uiDelegate: delegate, dataStore: store, bridge: delegate)
        if let script = options.probeUserScript {
            view.configuration.userContentController.addUserScript(WKUserScript(source: script, injectionTime: .atDocumentStart, forMainFrameOnly: true))
        }
        if options.probeMockCapture {
            _ = StudioWebViewFactory.callPrivateBoolSetter(view.configuration.preferences, "_setMockCaptureDevicesEnabled:", true)
            _ = StudioWebViewFactory.callPrivateBoolSetter(view, "_setWindowOcclusionDetectionEnabled:", false)
        }
        webView = view
        return view
    }

    private func makeWindow(content: WKWebView) {
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1280, height: 820),
                              styleMask: [.titled, .closable, .miniaturizable, .resizable],
                              backing: .buffered, defer: false)
        window.title = "p2p.video Studio"
        window.contentView = content
        window.contentMinSize = NSSize(width: 900, height: 600)
        window.collectionBehavior.insert(.fullScreenPrimary)
        window.isReleasedWhenClosed = false
        window.tabbingMode = .disallowed
        window.isRestorable = false
        window.delegate = self
        window.center()
        // Restores the last size and position (full screen is per launch).
        window.setFrameAutosaveName(Self.frameAutosaveName)
        self.window = window
    }

    // MARK: close

    func windowShouldClose(_ sender: NSWindow) -> Bool {
        if closeConfirmed { return true }
        let decision = StudioLifecycle.closeDecision(state: model?.hostState, window: windowState)
        lastCloseDecision = decision
        guard case .confirm(let message) = decision else { return true }
        if let answer = options.autoAnswerClose { return answer }
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "Close the studio window?"
        alert.informativeText = message
        alert.addButton(withTitle: "Keep Window Open")
        alert.addButton(withTitle: "Close Window")
        alert.beginSheetModal(for: sender) { [weak self] response in
            guard response == .alertSecondButtonReturn else { return }
            self?.closeConfirmed = true
            sender.performClose(nil)
        }
        return false
    }

    func windowWillClose(_ notification: Notification) {
        teardown()
    }

    /// Closes without asking (the user already confirmed quitting the app).
    func closeForQuit() {
        closeConfirmed = true
        window?.close()
    }

    /// Probe: the same decision as clicking the close button.
    func performClose() {
        guard let window, windowShouldClose(window) else { return }
        window.close()
    }

    private func teardown() {
        // Destroying the web view ends the page: the call is left, and the
        // recorder's committed fragments stay in the persistent store.
        webView?.stopLoading()
        // Unload the page now even if something still holds the view: the call,
        // capture and the recorder's Web Locks must end with the window.
        webView?.load(URLRequest(url: URL(string: "about:blank")!))
        webView?.configuration.userContentController.removeScriptMessageHandler(forName: StudioWebDelegate.bridgeName)
        webView?.removeFromSuperview()
        webView = nil
        delegate = nil
        window?.delegate = nil
        window = nil
        closeConfirmed = false
        inCall = false
        pageBadge = 0
        refreshActivity()
    }

    // MARK: page bridge, badge, notifications

    private func handle(_ message: StudioPageMessage) {
        pageMessages.append(message)
        switch message {
        case .call(let now):
            inCall = now
            refreshActivity()
        case .notify(let title, let body):
            let key = window?.isKeyWindow ?? false
            if DockBadge.shouldPostPageNotification(windowIsKey: key, appActive: NSApp.isActive) {
                model?.notifier.postPage(title: title, body: body)
            }
        case .badge(let count):
            pageBadge = count
            refreshBadge()
        }
    }

    private func navigationFinished(_ view: WKWebView) {
        // The daemon redirects ?launch= away; if an older daemon did not, drop
        // it from the page and history by loading the clean URL instead.
        if LaunchTokenHygiene.containsToken(view.url), let url = view.url {
            appLog.error("Studio: launch token still in the page URL; reloading without it")
            view.load(URLRequest(url: LaunchTokenHygiene.stripped(url)))
            return
        }
        onNavigationFinished?(view)
    }

    private func downloadFinished(_ url: URL) {
        finishedDownloads.append(url)
        // Bounces the Downloads stack in the Dock, as browsers do.
        DistributedNotificationCenter.default().post(name: Notification.Name("com.apple.DownloadFileFinished"), object: url.path)
        model?.notifier.postDownloadFinished(url)
    }

    // MARK: power management

    func refreshActivity(state: HostState?? = nil) {
        let next = StudioLifecycle.activity(state: state ?? model?.hostState, window: windowState)
        refreshBadge()
        guard next != activity else { return }
        activity = next
        if let token = activityToken { ProcessInfo.processInfo.endActivity(token); activityToken = nil }
        switch next {
        case .none:
            appLog.notice("Studio: App Nap allowed")
        case .call:
            activityToken = ProcessInfo.processInfo.beginActivity(
                options: [.userInitiatedAllowingIdleSystemSleep, .idleDisplaySleepDisabled, .latencyCritical],
                reason: "p2p.video call in progress")
            appLog.notice("Studio: call activity held (no App Nap, display awake)")
        case .capture:
            activityToken = ProcessInfo.processInfo.beginActivity(
                options: [.userInitiated, .idleSystemSleepDisabled, .idleDisplaySleepDisabled, .latencyCritical],
                reason: "p2p.video take recording or uploading")
            appLog.notice("Studio: capture activity held (no App Nap, no idle sleep)")
        }
    }

    var holdsActivityAssertion: Bool { activityToken != nil }

    private func refreshBadge() {
        let waiting = model?.hostState?.waitingCount ?? 0
        NSApp.dockTile.badgeLabel = DockBadge.label(pageBadge: window == nil ? 0 : pageBadge, waitingGuests: waiting)
    }
}
