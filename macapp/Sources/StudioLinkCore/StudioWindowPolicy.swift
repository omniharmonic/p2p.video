import CryptoKit
import Foundation

// Pure policy for the in-app studio window [SL-P2-3]: which origin may use
// the camera and microphone, where links and downloads go, what closing or
// quitting asks, and how much power management a call needs. The AppKit
// shell (StudioWindowController, StudioWebView) only applies these answers.

/// The daemon's own loopback origin: `http://127.0.0.1:<port>` (or `localhost`).
public struct DaemonOrigin: Equatable, Sendable {
    public var port: Int

    public init(port: Int) { self.port = port }

    public static let loopbackHosts: Set<String> = ["127.0.0.1", "localhost"]

    /// Matches a WebKit security origin (scheme, host, port; port 0 = default).
    public func matches(scheme: String, host: String, port: Int) -> Bool {
        scheme.lowercased() == "http" && Self.loopbackHosts.contains(host.lowercased()) && port == self.port
    }

    public func contains(_ url: URL) -> Bool {
        guard let scheme = url.scheme, let host = url.host else { return false }
        return matches(scheme: scheme, host: host, port: url.port ?? (scheme.lowercased() == "http" ? 80 : 443))
    }
}

/// Camera and microphone are granted to the daemon's own origin only, and only
/// when the requesting frame is that origin too (no third-party frame can ask).
public enum MediaCapturePolicy {
    public static func allows(origin: DaemonOrigin, requestScheme: String, requestHost: String, requestPort: Int,
                              frameURL: URL?) -> Bool {
        guard origin.matches(scheme: requestScheme, host: requestHost, port: requestPort) else { return false }
        // A frame without a URL yet (about:blank child) inherits the requester's
        // origin, which was checked above; any other frame must be the daemon.
        guard let frameURL, let scheme = frameURL.scheme?.lowercased(), scheme != "about" else { return true }
        return origin.contains(frameURL)
    }
}

/// Where a navigation in the studio web view goes.
public enum StudioNavigation: Equatable, Sendable {
    /// Load it in the studio window.
    case allowInApp
    /// Hand it to macOS (default browser, Mail, Calendar).
    case openExternally
    /// Save it with WKDownload into the Downloads folder.
    case download
    /// Refuse silently.
    case deny
}

public enum StudioNavigationPolicy {
    /// Schemes macOS opens in another app (default browser, Mail, Calendar).
    public static let externalSchemes: Set<String> = ["http", "https", "mailto", "webcal", "tel", "facetime"]

    /// - Parameters:
    ///   - isMainFrame: the target is the top-level page (false for iframes).
    ///   - opensNewWindow: `window.open` or a `target=_blank` link.
    ///   - shouldDownload: WebKit says this is a download (`<a download>`).
    public static func decide(_ url: URL, origin: DaemonOrigin, isMainFrame: Bool, opensNewWindow: Bool,
                              shouldDownload: Bool) -> StudioNavigation {
        let scheme = url.scheme?.lowercased() ?? ""
        let local = origin.contains(url)
        if shouldDownload {
            // Saved files come from the daemon or from the page itself (captions VTT).
            return local || scheme == "blob" || scheme == "data" ? .download : .deny
        }
        switch scheme {
        case "about", "blob", "data":
            // Same-document helpers; never as a new window.
            return opensNewWindow ? .deny : .allowInApp
        default:
            break
        }
        if local {
            // A daemon page asking for a new window (e.g. a guest link) belongs in
            // the browser: one studio window per host, and the call stays put.
            return opensNewWindow ? .openExternally : .allowInApp
        }
        guard externalSchemes.contains(scheme) else { return .deny }
        // Third-party content never loads inside the studio (no iframes either).
        return isMainFrame || opensNewWindow ? .openExternally : .deny
    }
}

/// File names for downloads from the studio window.
public enum DownloadNaming {
    public static let fallbackName = "p2p.video download"
    static let maxLength = 180

    /// A safe single path component from a server- or page-suggested name.
    public static func sanitize(_ suggested: String) -> String {
        var name = suggested.replacingOccurrences(of: "/", with: "-").replacingOccurrences(of: ":", with: "-")
        name = String(name.unicodeScalars.filter { !CharacterSet.controlCharacters.contains($0) }.map(Character.init))
        name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        while name.hasPrefix(".") { name.removeFirst() }
        name = name.trimmingCharacters(in: .whitespacesAndNewlines)
        if name.isEmpty { return fallbackName }
        if name.count > maxLength {
            let ext = (name as NSString).pathExtension
            let keep = maxLength - (ext.isEmpty ? 0 : ext.count + 1)
            let stem = String((name as NSString).deletingPathExtension.prefix(max(1, keep)))
            name = ext.isEmpty ? stem : "\(stem).\(ext)"
        }
        return name
    }

    /// `<dir>/<name>`, or `<dir>/<stem> (2).<ext>` … when that file exists.
    public static func destination(suggested: String, in directory: URL, exists: (URL) -> Bool) -> URL {
        let name = sanitize(suggested)
        let first = directory.appendingPathComponent(name, isDirectory: false)
        guard exists(first) else { return first }
        let ext = (name as NSString).pathExtension
        let stem = (name as NSString).deletingPathExtension
        for n in 2...9_999 {
            let candidate = directory.appendingPathComponent(ext.isEmpty ? "\(stem) (\(n))" : "\(stem) (\(n)).\(ext)", isDirectory: false)
            if !exists(candidate) { return candidate }
        }
        return directory.appendingPathComponent("\(stem) \(UUID().uuidString)\(ext.isEmpty ? "" : ".\(ext)")", isDirectory: false)
    }
}

/// The persistent WebKit data store behind the studio window. It is named
/// from the data directory, so OPFS upload journals survive window closes and
/// relaunches, and a test run with a temporary data directory never shares
/// (or clears) the real host's journals.
public enum StudioDataStoreIdentity {
    public static func identifier(dataDirectory: URL) -> UUID {
        let path = dataDirectory.standardizedFileURL.path
        let trimmed = path.count > 1 && path.hasSuffix("/") ? String(path.dropLast()) : path
        var bytes = Array(SHA256.hash(data: Data("com.omniharmonic.studiolink.webdata:\(trimmed)".utf8)).prefix(16))
        bytes[6] = (bytes[6] & 0x0F) | 0x50 // name-based (v5-style) UUID
        bytes[8] = (bytes[8] & 0x3F) | 0x80 // RFC 4122 variant
        return UUID(uuid: (bytes[0], bytes[1], bytes[2], bytes[3], bytes[4], bytes[5], bytes[6], bytes[7],
                           bytes[8], bytes[9], bytes[10], bytes[11], bytes[12], bytes[13], bytes[14], bytes[15]))
    }
}

/// The per-boot launch token must never stay in a URL the web view keeps
/// (current page, back/forward history) [SL-SEC-1].
public enum LaunchTokenHygiene {
    public static func containsToken(_ url: URL?) -> Bool {
        guard let url, let items = URLComponents(url: url, resolvingAgainstBaseURL: false)?.queryItems else { return false }
        return items.contains { $0.name == "launch" }
    }

    /// The same URL without `launch=`.
    public static func stripped(_ url: URL) -> URL {
        guard var c = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url }
        let items = (c.queryItems ?? []).filter { $0.name != "launch" }
        c.queryItems = items.isEmpty ? nil : items
        return c.url ?? url
    }
}

/// Lifecycle decisions for the studio window and the app around it.
public enum StudioLifecycle {
    /// Whether the studio page reports an active call (from the page bridge).
    public struct WindowState: Equatable, Sendable {
        public var open: Bool
        public var inCall: Bool

        public init(open: Bool, inCall: Bool) {
            self.open = open
            self.inCall = inCall
        }

        public static let closed = WindowState(open: false, inCall: false)
    }

    public enum CloseDecision: Equatable, Sendable {
        case close
        case confirm(message: String)
    }

    /// Closing the window ends the page: the host leaves the call and this
    /// window's recorder stops. Committed fragments stay in the WebKit store
    /// and upload when the studio is opened again.
    public static func closeDecision(state: HostState?, window: WindowState) -> CloseDecision {
        var parts: [String] = []
        if state?.recording == true { parts.append("A take is recording.") }
        if state?.live == true { parts.append("A livestream is running.") }
        if window.inCall || !(state?.activeCalls.isEmpty ?? true), parts.isEmpty { parts.append("You are in a call.") }
        if let pending = state?.uploadsPending, pending > 0 {
            parts.append("\(pending) recording upload\(pending == 1 ? " is" : "s are") still pending.")
        }
        guard window.inCall || state?.recording == true || state?.live == true || (state?.uploadsPending ?? 0) > 0 else {
            return .close
        }
        let consequence = "Closing this window leaves the call and stops recording on this Mac. "
            + "Anything already recorded stays on this Mac and finishes uploading when you open the studio again."
        return .confirm(message: (parts + [consequence]).joined(separator: " "))
    }

    /// Quitting stops the host for everyone; the studio window adds its own call.
    public static func quitMessage(state: HostState?, window: WindowState) -> String? {
        if let message = QuitWarning.message(state) { return message }
        return window.inCall ? "You are in a call in the studio window." : nil
    }

    /// Power management while the studio window is open.
    public enum Activity: Equatable, Sendable, Comparable {
        /// Nothing to protect: App Nap allowed.
        case none
        /// A call: no App Nap or timer throttling, the display stays awake.
        case call
        /// Recording, live or uploading: also keep the system from idle sleep.
        case capture
    }

    public static func activity(state: HostState?, window: WindowState) -> Activity {
        if state?.recording == true || state?.live == true || (state?.uploadsPending ?? 0) > 0 { return .capture }
        if window.open, window.inCall { return .call }
        return .none
    }

    public enum OpenDecision: Equatable, Sendable {
        /// Load the requested page in the (single) studio window.
        case load
        /// The same meeting is already open: bring the window forward, don't reload.
        case focusExisting
        /// A different meeting while in a call: ask before leaving it.
        case confirmReplace(message: String)
    }

    /// One studio window, one meeting at a time.
    public static func openDecision(current: URL?, requested: URL, window: WindowState) -> OpenDecision {
        guard window.open, let current else { return .load }
        if meetingKey(current) == meetingKey(requested) { return window.inCall ? .focusExisting : .load }
        guard window.inCall else { return .load }
        return .confirmReplace(message: "You are in a call in the studio window. Opening another meeting leaves this call.")
    }

    /// Identity of a studio page: path plus query, minus credentials.
    public static func meetingKey(_ url: URL) -> String {
        guard let c = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return url.path }
        let path = c.path.isEmpty ? "/" : c.path
        let items = (c.queryItems ?? []).filter { !["launch", "token"].contains($0.name) }
            .map { "\($0.name)=\($0.value ?? "")" }.sorted()
        return items.isEmpty ? path : "\(path)?\(items.joined(separator: "&"))"
    }
}

/// Messages the studio page's bridge script posts to the app.
public enum StudioPageMessage: Equatable, Sendable {
    case call(inCall: Bool)
    case notify(title: String, body: String)
    case badge(count: Int)

    /// Parses `window.webkit.messageHandlers.studiolink.postMessage(...)` bodies.
    public static func parse(_ body: Any) -> StudioPageMessage? {
        guard let dict = body as? [String: Any], let type = dict["type"] as? String else { return nil }
        switch type {
        case "call":
            guard let inCall = dict["inCall"] as? Bool else { return nil }
            return .call(inCall: inCall)
        case "notify":
            let title = String((dict["title"] as? String ?? "").prefix(200))
            guard !title.isEmpty else { return nil }
            return .notify(title: title, body: String((dict["body"] as? String ?? "").prefix(500)))
        case "badge":
            let n = (dict["count"] as? NSNumber)?.intValue ?? 0
            return .badge(count: max(0, min(n, 999)))
        default:
            return nil
        }
    }
}

/// Dock badge: what the page set, else guests waiting to be admitted.
public enum DockBadge {
    public static func label(pageBadge: Int, waitingGuests: Int) -> String? {
        if pageBadge > 0 { return String(pageBadge) }
        return waitingGuests > 0 ? String(waitingGuests) : nil
    }

    /// Page notifications are shown only when the studio window isn't in front
    /// (the page already shows its own toast there).
    public static func shouldPostPageNotification(windowIsKey: Bool, appActive: Bool) -> Bool {
        !(windowIsKey && appActive)
    }
}
