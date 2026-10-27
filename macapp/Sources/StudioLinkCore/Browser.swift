import Foundation

/// Which browser the app opens the dashboard in [SL-P2-2].
public enum DashboardBrowser: String, Codable, CaseIterable, Sendable {
    /// Google Chrome: the recommended, fully tested host browser.
    case chrome
    /// Whatever macOS opens http links with.
    case systemDefault

    public var label: String {
        switch self {
        case .chrome: return "Google Chrome"
        case .systemDefault: return "Default browser"
        }
    }
}

/// Where a dashboard link actually opens.
public enum BrowserTarget: Equatable, Sendable {
    case application(URL)
    case systemDefault
}

public enum BrowserDetection {
    public static let chromeBundleID = "com.google.Chrome"
    public static let chromeDownload = URL(string: "https://www.google.com/chrome/")!

    /// Standard install locations, system-wide first.
    public static func chromeCandidates(home: URL) -> [URL] {
        [URL(fileURLWithPath: "/Applications/Google Chrome.app", isDirectory: true),
         home.appendingPathComponent("Applications/Google Chrome.app", isDirectory: true)]
    }

    public static func locateChrome(home: URL, exists: (URL) -> Bool) -> URL? {
        chromeCandidates(home: home).first(where: exists)
    }

    /// The choice the guide preselects.
    public static func recommended(chromeInstalled: Bool) -> DashboardBrowser {
        chromeInstalled ? .chrome : .systemDefault
    }

    /// The saved preference (nil = never chosen) resolved against what is
    /// installed now. Chrome falls back to the default browser if it was removed.
    public static func effective(preference: DashboardBrowser?, chromeInstalled: Bool) -> DashboardBrowser {
        switch preference ?? recommended(chromeInstalled: chromeInstalled) {
        case .chrome: return chromeInstalled ? .chrome : .systemDefault
        case .systemDefault: return .systemDefault
        }
    }

    public static func target(preference: DashboardBrowser?, chrome: URL?) -> BrowserTarget {
        if effective(preference: preference, chromeInstalled: chrome != nil) == .chrome, let chrome {
            return .application(chrome)
        }
        return .systemDefault
    }
}
