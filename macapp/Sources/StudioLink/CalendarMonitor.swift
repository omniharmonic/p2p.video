import EventKit
import Foundation
import StudioLinkCore

/// EventKit behind `CalendarProvider` [SL-P6-1]. Calendars the user added to
/// macOS (Google, iCloud, Exchange) are read here; no calendar service is
/// called by p2p.video itself. Access is requested only from Settings.
@MainActor
final class EventKitCalendar: CalendarProvider {
    private let store = EKEventStore()

    var authorization: CalendarAuthorization {
        switch EKEventStore.authorizationStatus(for: .event) {
        case .notDetermined: return .notDetermined
        case .restricted: return .restricted
        case .denied: return .denied
        case .writeOnly: return .writeOnly
        case .fullAccess: return .fullAccess
        @unknown default: return .denied
        }
    }

    func requestFullAccess() async -> Bool {
        // Bare `swift run` binaries have no Info.plist usage string; macOS
        // would terminate them on request, so degrade instead.
        guard Bundle.main.object(forInfoDictionaryKey: "NSCalendarsFullAccessUsageDescription") != nil else { return false }
        let store = self.store
        let granted = await withCheckedContinuation { (cont: CheckedContinuation<Bool, Never>) in
            store.requestFullAccessToEvents { granted, _ in cont.resume(returning: granted) }
        }
        if granted { store.reset() }
        return granted
    }

    func events(from: Date, to: Date) -> [CalendarEventSnapshot] {
        guard authorization.canRead else { return [] }
        let predicate = store.predicateForEvents(withStart: from, end: to, calendars: nil)
        return store.events(matching: predicate).compactMap { e in
            guard let id = e.eventIdentifier, let start = e.startDate, let end = e.endDate else { return nil }
            return CalendarEventSnapshot(identifier: id, externalIdentifier: e.calendarItemExternalIdentifier, calendarTitle: e.calendar?.title ?? "Calendar",
                                         title: e.title ?? "", start: start, end: end, url: e.url, location: e.location, notes: e.notes)
        }
    }

    func writableCalendars() -> [WritableCalendar] {
        guard authorization.canRead else { return [] }
        return store.calendars(for: .event).filter(\.allowsContentModifications).map {
            WritableCalendar(id: $0.calendarIdentifier, title: $0.title, account: $0.source?.title ?? "")
        }.sorted { ($0.account, $0.title) < ($1.account, $1.title) }
    }

    func save(_ draft: ScheduledEventDraft, existing: String?) throws -> String {
        guard let calendar = store.calendar(withIdentifier: draft.calendarId) else { throw CalendarWriteError.calendarGone }
        // An event the user deleted by hand is simply created again.
        let event = existing.flatMap { store.event(withIdentifier: $0) } ?? EKEvent(eventStore: store)
        event.calendar = calendar
        event.title = draft.title
        event.startDate = draft.start
        event.endDate = draft.end
        event.notes = draft.notes
        event.url = draft.url
        event.location = draft.url?.absoluteString
        try store.save(event, span: .thisEvent, commit: true)
        guard let id = event.eventIdentifier else { throw CalendarWriteError.noIdentifier }
        return id
    }

    func remove(eventIdentifier: String) throws {
        guard let event = store.event(withIdentifier: eventIdentifier) else { return } // already gone
        try store.remove(event, span: .thisEvent, commit: true)
    }
}

enum CalendarWriteError: Error, CustomStringConvertible {
    case calendarGone, noIdentifier
    var description: String {
        switch self {
        case .calendarGone: return "The chosen calendar no longer exists. Pick another in Settings → Calendar."
        case .noIdentifier: return "The calendar did not keep the event."
        }
    }
}

/// Polls the daemon and the user's calendars once a minute while calendar
/// features are on: upcoming meetings for the menu, reminders, write-back.
@MainActor
final class CalendarMonitor: ObservableObject {
    @Published private(set) var upcoming: [UpcomingMeeting] = []
    @Published private(set) var authorization: CalendarAuthorization
    @Published private(set) var calendars: [WritableCalendar] = []
    @Published private(set) var status = ""

    let provider: CalendarProvider
    private var reminders = ReminderPlanner()
    private var timer: Timer?
    private let ledgerFile: URL

    init(provider: CalendarProvider = EventKitCalendar(), ledgerFile: URL) {
        self.provider = provider
        self.ledgerFile = ledgerFile
        authorization = provider.authorization
    }

    func start(tick: @escaping @MainActor () async -> Void) {
        timer?.invalidate()
        timer = Timer.scheduledTimer(withTimeInterval: 60, repeats: true) { _ in Task { @MainActor in await tick() } }
        Task { await tick() }
    }

    /// Only from an explicit Settings toggle.
    func requestAccess() async -> Bool {
        let ok = await provider.requestFullAccess()
        refreshAuthorization()
        return ok
    }

    func refreshAuthorization() {
        authorization = provider.authorization
        calendars = provider.writableCalendars()
    }

    /// One pass. Returns reminders to post.
    func refresh(settings: DesktopSettings, client: HostClient, desktopToken: String?) async -> [MeetingReminder] {
        refreshAuthorization()
        guard settings.calendarEnabled || settings.calendarWriteEnabled else {
            upcoming = []; status = ""
            return []
        }
        guard let token = desktopToken else { status = "Waiting for the host…"; return [] }
        let directory: DesktopCalendar
        do { directory = try await client.desktopCalendar(desktopToken: token) } catch {
            status = "Couldn’t read meetings from the host."
            return []
        }
        let now = Date()
        let events = settings.calendarEnabled ? provider.events(from: now.addingTimeInterval(-12 * 3600), to: now.addingTimeInterval(7 * 24 * 3600)) : []
        upcoming = UpcomingPlanner.merge(events: events, directory: directory, now: now)
        status = settings.calendarEnabled && !authorization.canRead ? authorization.explanation : ""

        if settings.calendarWriteEnabled, let calendarId = settings.calendarWriteCalendarID, authorization == .fullAccess {
            do {
                let meetings = try await client.scheduledMeetings(desktopToken: token)
                let ledger = CalendarSyncLedger.load(from: ledgerFile)
                let actions = CalendarSyncPlanner.plan(meetings: meetings, ledger: ledger, calendarId: calendarId, now: now)
                if !actions.isEmpty {
                    let (next, failures) = CalendarSyncRunner.apply(actions, provider: provider, ledger: ledger)
                    try next.save(to: ledgerFile)
                    appLog.notice("Calendar write-back: \(actions.count, privacy: .public) change(s), \(failures.count, privacy: .public) failed")
                    if let f = failures.first { status = f }
                }
            } catch {
                status = "Couldn’t update your calendar: \(error)"
            }
        }
        return settings.calendarEnabled ? reminders.due(upcoming, now: now, minutesBefore: settings.calendarReminderMinutes) : []
    }
}
