import AppKit
import Foundation
import StudioLinkCore
import UserNotifications

/// Posts local notifications. UserNotifications requires a bundle identifier,
/// so a bare `swift run` binary skips them (logged instead).
@MainActor
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    private var authorized: Bool?
    /// "Start meeting" on a meeting reminder [SL-P6-1]; receives the room ID.
    var onStartMeeting: ((String) -> Void)?

    nonisolated static let reminderCategory = "MEETING_REMINDER"
    nonisolated static let startAction = "START_MEETING"
    nonisolated static let downloadCategory = "STUDIO_DOWNLOAD"
    nonisolated static let revealAction = "SHOW_IN_FINDER"

    private var available: Bool { Bundle.main.bundleIdentifier != nil }

    override init() {
        super.init()
        guard available else { return }
        let center = UNUserNotificationCenter.current()
        center.delegate = self
        let start = UNNotificationAction(identifier: Self.startAction, title: "Start meeting", options: [.foreground])
        let reveal = UNNotificationAction(identifier: Self.revealAction, title: "Show in Finder", options: [])
        center.setNotificationCategories([
            UNNotificationCategory(identifier: Self.reminderCategory, actions: [start], intentIdentifiers: []),
            UNNotificationCategory(identifier: Self.downloadCategory, actions: [reveal], intentIdentifiers: []),
        ])
    }

    func requestAuthorization() {
        guard available, authorized == nil else { return }
        UNUserNotificationCenter.current().requestAuthorization(options: [.alert, .sound]) { granted, _ in
            Task { @MainActor in self.authorized = granted }
        }
    }

    func post(_ event: HostNotification) {
        appLog.notice("Notification: \(event.identifier, privacy: .public)")
        guard available else { return }
        requestAuthorization()
        let content = UNMutableNotificationContent()
        content.title = event.title
        content.body = event.body
        if case .guestWaiting = event { content.sound = .default }
        let request = UNNotificationRequest(identifier: event.identifier, content: content, trigger: nil)
        UNUserNotificationCenter.current().add(request)
    }

    /// A meeting is about to start; the action (or a click) opens its room.
    func post(_ reminder: MeetingReminder) {
        appLog.notice("Meeting reminder: \(reminder.identifier, privacy: .public)")
        guard available else { return }
        requestAuthorization()
        let content = UNMutableNotificationContent()
        content.title = reminder.title
        content.body = reminder.body
        content.sound = .default
        content.categoryIdentifier = Self.reminderCategory
        content.userInfo = ["roomId": reminder.roomId]
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: reminder.identifier, content: content, trigger: nil))
    }

    /// A studio-window download landed (Show in Finder reveals it) [SL-P2-3].
    func postDownloadFinished(_ file: URL) {
        appLog.notice("Notification: download finished")
        guard available else { return }
        requestAuthorization()
        let content = UNMutableNotificationContent()
        content.title = "Downloaded \(file.lastPathComponent)"
        content.body = "Saved to \(file.deletingLastPathComponent().lastPathComponent). Click to show it in Finder."
        content.categoryIdentifier = Self.downloadCategory
        content.userInfo = ["revealPath": file.path]
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: "download-\(UUID().uuidString)", content: content, trigger: nil))
    }

    func postDownloadFailed(name: String, reason: String) {
        guard available else { return }
        requestAuthorization()
        let content = UNMutableNotificationContent()
        content.title = "Download failed"
        content.body = "\(name): \(reason)"
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: "download-failed-\(UUID().uuidString)", content: content, trigger: nil))
    }

    /// A Web Notification from the studio page (WKWebView has no Notification API).
    func postPage(title: String, body: String) {
        appLog.notice("Notification: studio page")
        guard available else { return }
        requestAuthorization()
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        UNUserNotificationCenter.current().add(UNNotificationRequest(identifier: "page-\(UUID().uuidString)", content: content, trigger: nil))
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                            withCompletionHandler completionHandler: @escaping () -> Void) {
        if let path = response.notification.request.content.userInfo["revealPath"] as? String {
            completionHandler()
            let url = URL(fileURLWithPath: path)
            Task { @MainActor in NSWorkspace.shared.activateFileViewerSelecting([url]) }
            return
        }
        let roomId = response.notification.request.content.userInfo["roomId"] as? String
        let action = response.actionIdentifier
        completionHandler()
        guard let roomId, action == Self.startAction || action == UNNotificationDefaultActionIdentifier else { return }
        Task { @MainActor in self.onStartMeeting?(roomId) }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                            withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .sound])
    }
}
