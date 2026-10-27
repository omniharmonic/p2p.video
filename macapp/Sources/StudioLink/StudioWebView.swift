import AppKit
import ObjectiveC
import StudioLinkCore
import WebKit

/// Grants camera/microphone to pages on this host's own loopback origin only.
/// macOS still asks the user once (TCC) for the app itself. Used by the
/// capability probe; the studio window uses `StudioWebDelegate`, which adds
/// navigation, downloads and the page bridge on top of the same policy.
@MainActor
class LoopbackMediaUIDelegate: NSObject, WKUIDelegate {
    let origin: DaemonOrigin
    private(set) var permissionRequests = 0

    init(port: Int) { origin = DaemonOrigin(port: port) }

    func webView(_ webView: WKWebView, requestMediaCapturePermissionFor origin: WKSecurityOrigin,
                 initiatedByFrame frame: WKFrameInfo, type: WKMediaCaptureType,
                 decisionHandler: @escaping @MainActor (WKPermissionDecision) -> Void) {
        permissionRequests += 1
        let ok = MediaCapturePolicy.allows(origin: self.origin, requestScheme: origin.protocol, requestHost: origin.host,
                                           requestPort: origin.port, frameURL: frame.request.url)
        if !ok { appLog.notice("Studio: camera/microphone denied to a non-studio origin") }
        decisionHandler(ok ? .grant : .deny)
    }

    func webView(_ webView: WKWebView, runJavaScriptAlertPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping @MainActor () -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        if let window = webView.window {
            alert.beginSheetModal(for: window) { _ in completionHandler() }
        } else {
            alert.runModal()
            completionHandler()
        }
    }

    func webView(_ webView: WKWebView, runJavaScriptConfirmPanelWithMessage message: String,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping @MainActor (Bool) -> Void) {
        let alert = NSAlert()
        alert.messageText = message
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        if let window = webView.window {
            alert.beginSheetModal(for: window) { completionHandler($0 == .alertFirstButtonReturn) }
        } else {
            completionHandler(alert.runModal() == .alertFirstButtonReturn)
        }
    }

    func webView(_ webView: WKWebView, runJavaScriptTextInputPanelWithPrompt prompt: String, defaultText: String?,
                 initiatedByFrame frame: WKFrameInfo, completionHandler: @escaping @MainActor (String?) -> Void) {
        let alert = NSAlert()
        alert.messageText = prompt
        let field = NSTextField(frame: NSRect(x: 0, y: 0, width: 280, height: 24))
        field.stringValue = defaultText ?? ""
        alert.accessoryView = field
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Cancel")
        completionHandler(alert.runModal() == .alertFirstButtonReturn ? field.stringValue : nil)
    }

    /// `<input type=file>` (branding logos, lower-third images).
    func webView(_ webView: WKWebView, runOpenPanelWith parameters: WKOpenPanelParameters, initiatedByFrame frame: WKFrameInfo,
                 completionHandler: @escaping @MainActor ([URL]?) -> Void) {
        let panel = NSOpenPanel()
        panel.allowsMultipleSelection = parameters.allowsMultipleSelection
        panel.canChooseDirectories = parameters.allowsDirectories
        panel.canChooseFiles = true
        completionHandler(panel.runModal() == .OK ? panel.urls : nil)
    }
}

/// Everything the studio window needs from WebKit: media permission, link and
/// download routing, and the page bridge. URLs are never logged (invite and
/// host links carry tokens).
@MainActor
final class StudioWebDelegate: LoopbackMediaUIDelegate, WKNavigationDelegate, WKDownloadDelegate, WKScriptMessageHandler {
    static let bridgeName = "studiolink"

    /// Where a link the studio must not load goes (default: NSWorkspace).
    var openExternally: (URL) -> Void = { NSWorkspace.shared.open($0) }
    var downloadsDirectory: () -> URL = {
        FileManager.default.urls(for: .downloadsDirectory, in: .userDomainMask).first
            ?? FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Downloads", isDirectory: true)
    }
    var onDownloadFinished: (URL) -> Void = { _ in }
    var onDownloadFailed: (String, String) -> Void = { _, _ in }
    var onPageMessage: (StudioPageMessage) -> Void = { _ in }
    var onNavigationFinished: (WKWebView) -> Void = { _ in }
    var onContentProcessTerminated: (WKWebView) -> Void = { _ in }

    private var destinations: [ObjectIdentifier: URL] = [:]
    private(set) var externalOpens = 0
    private(set) var deniedNavigations = 0

    // MARK: navigation

    func webView(_ webView: WKWebView, decidePolicyFor navigationAction: WKNavigationAction,
                 decisionHandler: @escaping @MainActor (WKNavigationActionPolicy) -> Void) {
        guard let url = navigationAction.request.url else { decisionHandler(.cancel); return }
        let decision = StudioNavigationPolicy.decide(url, origin: origin, isMainFrame: navigationAction.targetFrame?.isMainFrame ?? true,
                                                     opensNewWindow: navigationAction.targetFrame == nil,
                                                     shouldDownload: navigationAction.shouldPerformDownload)
        apply(decision, url: url, allow: { decisionHandler(.allow) }, download: { decisionHandler(.download) },
              cancel: { decisionHandler(.cancel) })
    }

    func webView(_ webView: WKWebView, decidePolicyFor navigationResponse: WKNavigationResponse,
                 decisionHandler: @escaping @MainActor (WKNavigationResponsePolicy) -> Void) {
        let disposition = (navigationResponse.response as? HTTPURLResponse)?.value(forHTTPHeaderField: "Content-Disposition") ?? ""
        if !navigationResponse.canShowMIMEType || disposition.lowercased().hasPrefix("attachment") {
            decisionHandler(.download)
        } else {
            decisionHandler(.allow)
        }
    }

    func webView(_ webView: WKWebView, navigationAction: WKNavigationAction, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, navigationResponse: WKNavigationResponse, didBecome download: WKDownload) {
        download.delegate = self
    }

    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        onNavigationFinished(webView)
    }

    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) {
        appLog.error("Studio: the web content process ended; reloading")
        onContentProcessTerminated(webView)
    }

    /// `window.open` and `target=_blank`: never a second web view. (Explicit
    /// selector: the WKUIDelegate conformance is declared on the superclass,
    /// so Swift would not expose this override to WebKit on its own.)
    @objc(webView:createWebViewWithConfiguration:forNavigationAction:windowFeatures:)
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration,
                 for navigationAction: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        guard let url = navigationAction.request.url else { return nil }
        let decision = StudioNavigationPolicy.decide(url, origin: origin, isMainFrame: true, opensNewWindow: true,
                                                     shouldDownload: navigationAction.shouldPerformDownload)
        if decision == .download {
            webView.startDownload(using: navigationAction.request) { $0.delegate = self }
        } else {
            apply(decision, url: url, allow: {}, download: {}, cancel: {})
        }
        return nil
    }

    private func apply(_ decision: StudioNavigation, url: URL, allow: () -> Void, download: () -> Void, cancel: () -> Void) {
        switch decision {
        case .allowInApp: allow()
        case .download: download()
        case .openExternally:
            cancel()
            externalOpens += 1
            appLog.notice("Studio: opening a \(url.scheme ?? "?", privacy: .public) link outside the app")
            openExternally(url)
        case .deny:
            cancel()
            deniedNavigations += 1
            appLog.notice("Studio: refused a \(url.scheme ?? "?", privacy: .public) navigation")
        }
    }

    // MARK: downloads

    func download(_ download: WKDownload, decideDestinationUsing response: URLResponse, suggestedFilename: String,
                  completionHandler: @escaping @MainActor (URL?) -> Void) {
        if let http = response as? HTTPURLResponse, http.statusCode >= 400 {
            // An error page is not a file: report it instead of saving it.
            appLog.error("Studio: download refused, HTTP \(http.statusCode, privacy: .public)")
            onDownloadFailed(suggestedFilename, "the host answered HTTP \(http.statusCode)")
            completionHandler(nil)
            return
        }
        let directory = downloadsDirectory()
        try? FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        let destination = DownloadNaming.destination(suggested: suggestedFilename, in: directory) {
            FileManager.default.fileExists(atPath: $0.path)
        }
        destinations[ObjectIdentifier(download)] = destination
        completionHandler(destination)
    }

    func downloadDidFinish(_ download: WKDownload) {
        guard let destination = destinations.removeValue(forKey: ObjectIdentifier(download)) else { return }
        appLog.notice("Studio: download finished")
        onDownloadFinished(destination)
    }

    func download(_ download: WKDownload, didFailWithError error: Error, resumeData: Data?) {
        let name = destinations.removeValue(forKey: ObjectIdentifier(download))?.lastPathComponent ?? DownloadNaming.fallbackName
        appLog.error("Studio: download failed: \(error.localizedDescription, privacy: .public)")
        onDownloadFailed(name, error.localizedDescription)
    }

    // MARK: page bridge

    nonisolated func userContentController(_ controller: WKUserContentController, didReceive message: WKScriptMessage) {
        MainActor.assumeIsolated {
            let o = message.frameInfo.securityOrigin
            guard message.frameInfo.isMainFrame, origin.matches(scheme: o.protocol, host: o.host, port: o.port),
                  let parsed = StudioPageMessage.parse(message.body) else { return }
            onPageMessage(parsed)
        }
    }
}

enum StudioWebViewFactory {
    /// The page bridge: reports whether the page shows the call (so closing
    /// and quitting can ask first), and maps the Notifications and Badging
    /// APIs, which WKWebView lacks, to the app.
    static let bridgeScript = #"""
(() => {
  const post = m => { try { window.webkit.messageHandlers.studiolink.postMessage(m); } catch (_) {} };
  let last = null;
  const check = () => {
    const v = document.getElementById('studio-view');
    const inCall = !!v && v.getClientRects().length > 0 && getComputedStyle(v).visibility !== 'hidden';
    if (inCall !== last) { last = inCall; post({type: 'call', inCall}); }
  };
  setInterval(check, 1000);
  document.addEventListener('DOMContentLoaded', check);
  window.addEventListener('pagehide', () => post({type: 'call', inCall: false}));
  {
    // WKWebView's own Notification never displays: route it to the app.
    class StudioNotification extends EventTarget {
      constructor(title, options = {}) {
        super(); this.title = String(title); this.body = String(options.body || '');
        post({type: 'notify', title: this.title, body: this.body});
      }
      static get permission() { return 'granted'; }
      static requestPermission(cb) { if (typeof cb === 'function') cb('granted'); return Promise.resolve('granted'); }
      close() {}
    }
    window.Notification = StudioNotification;
  }
  navigator.setAppBadge = n => { post({type: 'badge', count: n === undefined ? 1 : Number(n) || 0}); return Promise.resolve(); };
  navigator.clearAppBadge = () => { post({type: 'badge', count: 0}); return Promise.resolve(); };
})();
"""#

    @MainActor
    static func make(uiDelegate: WKUIDelegate, dataStore: WKWebsiteDataStore = .default(),
                     bridge: StudioWebDelegate? = nil) -> WKWebView {
        let config = WKWebViewConfiguration()
        // Persistent (never .nonPersistent()): OPFS upload journals must outlive
        // a closed window or a crash so uploads resume on the next open.
        config.websiteDataStore = dataStore
        config.mediaTypesRequiringUserActionForPlayback = []
        config.preferences.isElementFullscreenEnabled = true
        config.preferences.javaScriptCanOpenWindowsAutomatically = false
        config.applicationNameForUserAgent = "p2p.video"
        if let bridge {
            config.userContentController.addUserScript(WKUserScript(source: bridgeScript, injectionTime: .atDocumentStart, forMainFrameOnly: true))
            config.userContentController.add(bridge, name: StudioWebDelegate.bridgeName)
        }
        let view = WKWebView(frame: NSRect(x: 0, y: 0, width: 1200, height: 800), configuration: config)
        view.uiDelegate = uiDelegate
        if let bridge { view.navigationDelegate = bridge }
        view.allowsBackForwardNavigationGestures = false
        view.allowsMagnification = false
        return view
    }

    /// The studio's named persistent store (see `StudioDataStoreIdentity`).
    @MainActor
    static func dataStore(dataDirectory: URL) -> WKWebsiteDataStore {
        WKWebsiteDataStore(forIdentifier: StudioDataStoreIdentity.identifier(dataDirectory: dataDirectory))
    }

    /// Calls a private Bool setter (`_setX:`) if the object implements it.
    /// Probe-only: the shipping studio window uses no private WebKit API.
    @discardableResult
    static func callPrivateBoolSetter(_ object: NSObject, _ selector: String, _ value: Bool) -> Bool {
        let sel = NSSelectorFromString(selector)
        guard object.responds(to: sel), let method = class_getInstanceMethod(type(of: object), sel) else {
            appLog.error("Probe: \(selector, privacy: .public) unavailable")
            return false
        }
        typealias Setter = @convention(c) (AnyObject, Selector, Bool) -> Void
        unsafeBitCast(method_getImplementation(method), to: Setter.self)(object, sel, value)
        return true
    }
}
