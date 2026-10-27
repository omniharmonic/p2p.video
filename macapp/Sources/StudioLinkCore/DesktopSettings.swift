import Foundation

/// How guests reach this host. Chosen explicitly by the user in Settings.
public enum GuestAccessMode: String, Codable, Sendable, CaseIterable {
    /// Loopback only; nothing is published through Tailscale by the app.
    case off
    /// Tailscale Serve on :443, reachable only inside the tailnet.
    case privateTailnet
    /// Tailscale Funnel on :443 plus TURN over Funnel :8443 (anyone with the link).
    case publicFunnel
}

/// App settings persisted in the data directory (`desktop.json`), so a
/// temporary data directory isolates them completely.
public struct DesktopSettings: Codable, Equatable, Sendable {
    public var guestAccess: GuestAccessMode
    /// `https://<host>.<tailnet>.ts.net`, recorded when Serve/Funnel was set up.
    public var publicURL: String?
    /// Public STUN for direct UDP (ADR-005). Policy-controlled and off-able.
    public var publicSTUN: Bool
    public var notifyGuestWaiting: Bool
    public var notifyTakeProgress: Bool
    /// Open the in-app studio (true) or the default browser (false).
    public var studioInApp: Bool
    /// Run the on-device live-captions recognizer for the host [SL-P4-2].
    /// Captions are still off in every meeting until the host turns them on.
    public var captionProvider: Bool
    /// Run the on-device file recognizer for post-take transcripts (the
    /// "Built-in" engine) [SL-P4-3]. Nothing runs until the host transcribes a take.
    public var transcriptionProvider: Bool
    /// Browser for dashboard pages; nil = never chosen (Chrome when installed) [SL-P2-2].
    public var dashboardBrowser: DashboardBrowser?
    /// The first-run setup guide was finished or skipped [SL-P2-2].
    public var onboardingCompleted: Bool
    /// Read the user's calendars (EventKit) for meetings with a room link
    /// [SL-P6-1]. Off by default; turning it on asks macOS for access.
    public var calendarEnabled: Bool
    /// Remind this many minutes before a meeting (0 = no reminder).
    public var calendarReminderMinutes: Int
    /// Mirror scheduled meetings into `calendarWriteCalendarID`. Off by default.
    public var calendarWriteEnabled: Bool
    public var calendarWriteCalendarID: String?

    public init(guestAccess: GuestAccessMode = .off, publicURL: String? = nil, publicSTUN: Bool = true,
                notifyGuestWaiting: Bool = true, notifyTakeProgress: Bool = true, studioInApp: Bool = false,
                captionProvider: Bool = true, dashboardBrowser: DashboardBrowser? = nil, onboardingCompleted: Bool = false,
                calendarEnabled: Bool = false, calendarReminderMinutes: Int = 5, calendarWriteEnabled: Bool = false, calendarWriteCalendarID: String? = nil,
                transcriptionProvider: Bool = true) {
        self.calendarEnabled = calendarEnabled
        self.calendarReminderMinutes = calendarReminderMinutes
        self.calendarWriteEnabled = calendarWriteEnabled
        self.calendarWriteCalendarID = calendarWriteCalendarID
        self.transcriptionProvider = transcriptionProvider
        self.dashboardBrowser = dashboardBrowser
        self.onboardingCompleted = onboardingCompleted
        self.guestAccess = guestAccess
        self.publicURL = publicURL
        self.publicSTUN = publicSTUN
        self.notifyGuestWaiting = notifyGuestWaiting
        self.notifyTakeProgress = notifyTakeProgress
        self.studioInApp = studioInApp
        self.captionProvider = captionProvider
    }

    enum CodingKeys: String, CodingKey { case guestAccess, publicURL, publicSTUN, notifyGuestWaiting, notifyTakeProgress, studioInApp, captionProvider, dashboardBrowser, onboardingCompleted,
                             calendarEnabled, calendarReminderMinutes, calendarWriteEnabled, calendarWriteCalendarID, transcriptionProvider }

    // Tolerant decoding: settings written by an older app keep working.
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let d = DesktopSettings()
        guestAccess = (try? c.decode(GuestAccessMode.self, forKey: .guestAccess)) ?? d.guestAccess
        publicURL = try? c.decodeIfPresent(String.self, forKey: .publicURL)
        publicSTUN = (try? c.decode(Bool.self, forKey: .publicSTUN)) ?? d.publicSTUN
        notifyGuestWaiting = (try? c.decode(Bool.self, forKey: .notifyGuestWaiting)) ?? d.notifyGuestWaiting
        notifyTakeProgress = (try? c.decode(Bool.self, forKey: .notifyTakeProgress)) ?? d.notifyTakeProgress
        studioInApp = (try? c.decode(Bool.self, forKey: .studioInApp)) ?? d.studioInApp
        captionProvider = (try? c.decode(Bool.self, forKey: .captionProvider)) ?? d.captionProvider
        transcriptionProvider = (try? c.decode(Bool.self, forKey: .transcriptionProvider)) ?? d.transcriptionProvider
        dashboardBrowser = try? c.decodeIfPresent(DashboardBrowser.self, forKey: .dashboardBrowser)
        onboardingCompleted = (try? c.decode(Bool.self, forKey: .onboardingCompleted)) ?? d.onboardingCompleted
        calendarEnabled = (try? c.decode(Bool.self, forKey: .calendarEnabled)) ?? d.calendarEnabled
        calendarReminderMinutes = max(0, min(120, (try? c.decode(Int.self, forKey: .calendarReminderMinutes)) ?? d.calendarReminderMinutes))
        calendarWriteEnabled = (try? c.decode(Bool.self, forKey: .calendarWriteEnabled)) ?? d.calendarWriteEnabled
        calendarWriteCalendarID = try? c.decodeIfPresent(String.self, forKey: .calendarWriteCalendarID)
    }

    /// Loads settings; a missing file yields defaults, with the launcher's
    /// `public-url.txt` honored as a private-mode public URL.
    public static func load(from file: URL, legacyPublicURL: URL? = nil) -> DesktopSettings {
        if let data = try? Data(contentsOf: file), let s = try? JSONDecoder().decode(DesktopSettings.self, from: data) {
            return s
        }
        var s = DesktopSettings()
        if let legacy = legacyPublicURL,
           let text = try? String(contentsOf: legacy, encoding: .utf8).trimmingCharacters(in: .whitespacesAndNewlines),
           !text.isEmpty {
            s.publicURL = text
        }
        return s
    }

    /// Writes settings atomically with owner-only permissions.
    public func save(to file: URL) throws {
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        let data = try encoder.encode(self)
        try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        try data.write(to: file, options: [.atomic])
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
    }
}
