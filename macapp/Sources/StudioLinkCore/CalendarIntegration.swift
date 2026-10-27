import Foundation

// Calendar integration for the host app [SL-P6-1]. Pure logic only: the
// EventKit glue lives in the app target behind `CalendarProvider`, so every
// rule here is unit-tested with fakes and never touches a real calendar.
//
//  - Matching: a calendar event is a p2p.video meeting when its URL,
//    location or notes hold `<one of this host's origins>/join?room=<slug>`
//    for one of the daemon's rooms. Only the slug and origin are compared;
//    the link token is never kept.
//  - Upcoming: matched events merged with the daemon's scheduled meetings.
//  - Reminders: N minutes before a start, once per meeting start.
//  - Write-back (opt-in): scheduled meetings mirrored into one chosen
//    calendar, tracked in a ledger so edits update and cancels remove.

// MARK: daemon data

/// Shared decoder for the daemon's RFC 3339 times (with or without fractions).
enum DaemonJSON {
    static func decoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { decoder in
            let c = try decoder.singleValueContainer()
            let s = try c.decode(String.self)
            if let date = parse(s) { return date }
            throw DecodingError.dataCorruptedError(in: c, debugDescription: "Not an RFC 3339 time: \(s)")
        }
        return d
    }

    static func parse(_ s: String) -> Date? {
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        if let d = plain.date(from: s) { return d }
        // Go writes up to nanoseconds; Foundation reads up to milliseconds.
        let trimmed = s.replacingOccurrences(of: #"(\.\d{3})\d+"#, with: "$1", options: .regularExpression)
        let frac = ISO8601DateFormatter()
        frac.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return frac.date(from: trimmed)
    }
}

/// Mirror of `GET /api/v1/desktop/calendar`: rooms (slugs), the origins this
/// host's guest links use, and scheduled meeting times. No secrets.
public struct DesktopCalendar: Decodable, Equatable, Sendable {
    public struct Room: Decodable, Equatable, Sendable {
        public var id: String
        public var name: String
        public var slug: String
        public var live: Bool
        public init(id: String, name: String, slug: String, live: Bool = false) {
            self.id = id; self.name = name; self.slug = slug; self.live = live
        }
    }

    public struct Scheduled: Decodable, Equatable, Sendable {
        public var id: String
        public var roomId: String
        public var title: String
        public var startsAt: Date
        public var endsAt: Date
        public var status: String
        public var sequence: Int
        public var uid: String
        public init(id: String, roomId: String, title: String, startsAt: Date, endsAt: Date, status: String = "scheduled", sequence: Int = 0, uid: String) {
            self.id = id; self.roomId = roomId; self.title = title; self.startsAt = startsAt; self.endsAt = endsAt
            self.status = status; self.sequence = sequence; self.uid = uid
        }
        public var cancelled: Bool { status == "cancelled" }
    }

    public var rooms: [Room]
    public var guestOrigins: [String]
    public var scheduled: [Scheduled]

    public init(rooms: [Room], guestOrigins: [String], scheduled: [Scheduled]) {
        self.rooms = rooms; self.guestOrigins = guestOrigins; self.scheduled = scheduled
    }

    public static func decode(_ data: Data) throws -> DesktopCalendar {
        try DaemonJSON.decoder().decode(DesktopCalendar.self, from: data)
    }
}

/// A scheduled meeting with its invite, from `GET /api/v1/schedule` (desktop
/// token). `inviteText` and `guestURL` carry the room link secret: they go
/// only into the user's chosen calendar, never into logs.
public struct ScheduledMeetingInvite: Decodable, Equatable, Sendable {
    public struct Invite: Decodable, Equatable, Sendable {
        public var text: String
        public var guestUrl: String?
        public init(text: String, guestUrl: String?) { self.text = text; self.guestUrl = guestUrl }
    }
    public var id: String
    public var roomId: String
    public var title: String
    public var startsAt: Date
    public var endsAt: Date
    public var status: String
    public var sequence: Int
    public var linkReady: Bool
    public var invite: Invite

    public init(id: String, roomId: String, title: String, startsAt: Date, endsAt: Date, status: String = "scheduled", sequence: Int = 0, linkReady: Bool = true, invite: Invite) {
        self.id = id; self.roomId = roomId; self.title = title; self.startsAt = startsAt; self.endsAt = endsAt
        self.status = status; self.sequence = sequence; self.linkReady = linkReady; self.invite = invite
    }

    public var cancelled: Bool { status == "cancelled" }

    public static func decodeList(_ data: Data) throws -> [ScheduledMeetingInvite] {
        struct List: Decodable { var meetings: [ScheduledMeetingInvite] }
        return try DaemonJSON.decoder().decode(List.self, from: data).meetings
    }
}

// MARK: calendar events

/// What the app reads from one calendar event (EventKit-independent).
public struct CalendarEventSnapshot: Equatable, Sendable {
    public var identifier: String
    /// The iCalendar UID where the store keeps it (imported .ics, CalDAV).
    public var externalIdentifier: String?
    public var calendarTitle: String
    public var title: String
    public var start: Date
    public var end: Date
    public var url: URL?
    public var location: String?
    public var notes: String?

    public init(identifier: String, externalIdentifier: String? = nil, calendarTitle: String = "Calendar", title: String,
                start: Date, end: Date, url: URL? = nil, location: String? = nil, notes: String? = nil) {
        self.identifier = identifier; self.externalIdentifier = externalIdentifier; self.calendarTitle = calendarTitle
        self.title = title; self.start = start; self.end = end; self.url = url; self.location = location; self.notes = notes
    }
}

/// The room a calendar event's link points at.
public struct RoomLinkMatch: Equatable, Sendable {
    public var roomId: String
    public var roomName: String
    public var slug: String
    public init(roomId: String, roomName: String, slug: String) { self.roomId = roomId; self.roomName = roomName; self.slug = slug }
}

public enum RoomLinkMatcher {
    private static let urlPattern = try! NSRegularExpression(pattern: #"https?://[^\s<>"'\\]+"#, options: [.caseInsensitive])

    /// Every http(s) URL in free text (notes, location), trailing punctuation trimmed.
    public static func urls(in text: String) -> [URL] {
        let range = NSRange(text.startIndex..., in: text)
        return urlPattern.matches(in: text, range: range).compactMap { m in
            guard let r = Range(m.range, in: text) else { return nil }
            var s = String(text[r])
            while let last = s.last, ".,;:)]}!?".contains(last) { s.removeLast() }
            return URL(string: s)
        }
    }

    /// `scheme://host[:port]`, lowercased, default ports dropped.
    public static func origin(of url: URL) -> String? {
        guard let scheme = url.scheme?.lowercased(), let host = url.host?.lowercased(), !host.isEmpty else { return nil }
        var o = "\(scheme)://\(host)"
        if let port = url.port, !(scheme == "https" && port == 443), !(scheme == "http" && port == 80) { o += ":\(port)" }
        return o
    }

    /// The room a URL is a guest link for, if it is this host's.
    public static func match(_ url: URL, in directory: DesktopCalendar) -> RoomLinkMatch? {
        guard let origin = origin(of: url), url.path == "/join",
              let comps = URLComponents(url: url, resolvingAgainstBaseURL: false),
              let slug = comps.queryItems?.first(where: { $0.name == "room" })?.value, !slug.isEmpty else { return nil }
        let origins = Set(directory.guestOrigins.compactMap { URL(string: $0).flatMap(origin(of:)) })
        guard origins.contains(origin), let room = directory.rooms.first(where: { $0.slug == slug }) else { return nil }
        return RoomLinkMatch(roomId: room.id, roomName: room.name, slug: slug)
    }

    /// Looks at the event's URL, then location, then notes.
    public static func match(event: CalendarEventSnapshot, in directory: DesktopCalendar) -> RoomLinkMatch? {
        var candidates: [URL] = []
        if let u = event.url { candidates.append(u) }
        if let l = event.location { candidates += urls(in: l) }
        if let n = event.notes { candidates += urls(in: n) }
        for u in candidates { if let m = match(u, in: directory) { return m } }
        return nil
    }
}

// MARK: upcoming

/// A meeting the app shows and reminds about.
public struct UpcomingMeeting: Equatable, Sendable, Identifiable {
    public enum Source: Equatable, Sendable {
        case scheduled
        case calendar(String)
    }
    public var id: String
    public var roomId: String
    public var roomName: String
    public var title: String
    public var start: Date
    public var end: Date
    public var source: Source
    public var scheduledMeetingId: String?

    public init(id: String, roomId: String, roomName: String, title: String, start: Date, end: Date, source: Source, scheduledMeetingId: String?) {
        self.id = id; self.roomId = roomId; self.roomName = roomName; self.title = title
        self.start = start; self.end = end; self.source = source; self.scheduledMeetingId = scheduledMeetingId
    }
}

public enum UpcomingPlanner {
    /// Matched calendar events plus the daemon's scheduled meetings that end
    /// after `now` and start within `horizon`, earliest first. A calendar copy
    /// of a scheduled meeting (same UID, or same room within a minute) shows once.
    public static func merge(events: [CalendarEventSnapshot], directory: DesktopCalendar, now: Date,
                             horizon: TimeInterval = 7 * 24 * 3600) -> [UpcomingMeeting] {
        let names = Dictionary(directory.rooms.map { ($0.id, $0.name) }, uniquingKeysWith: { a, _ in a })
        var out: [UpcomingMeeting] = []
        let scheduled = directory.scheduled.filter { !$0.cancelled && $0.endsAt > now && $0.startsAt < now.addingTimeInterval(horizon) }
        for s in scheduled {
            out.append(UpcomingMeeting(id: "scheduled:\(s.id)", roomId: s.roomId, roomName: names[s.roomId] ?? "Room", title: s.title,
                                       start: s.startsAt, end: s.endsAt, source: .scheduled, scheduledMeetingId: s.id))
        }
        let cancelledUIDs = Set(directory.scheduled.filter(\.cancelled).map(\.uid))
        for e in events where e.end > now && e.start < now.addingTimeInterval(horizon) {
            guard let m = RoomLinkMatcher.match(event: e, in: directory) else { continue }
            if let uid = e.externalIdentifier, cancelledUIDs.contains(uid) { continue }
            let duplicate = scheduled.contains { s in
                (e.externalIdentifier != nil && e.externalIdentifier == s.uid) || (s.roomId == m.roomId && abs(s.startsAt.timeIntervalSince(e.start)) < 60)
            }
            if duplicate { continue }
            out.append(UpcomingMeeting(id: "event:\(e.identifier):\(Int(e.start.timeIntervalSince1970))", roomId: m.roomId, roomName: m.roomName,
                                       title: e.title.isEmpty ? m.roomName : e.title, start: e.start, end: e.end,
                                       source: .calendar(e.calendarTitle), scheduledMeetingId: nil))
        }
        return out.sorted { ($0.start, $0.id) < ($1.start, $1.id) }
    }
}

// MARK: reminders

/// A "meeting starts soon" notification with a Start action.
public struct MeetingReminder: Equatable, Sendable {
    public var identifier: String
    public var roomId: String
    public var title: String
    public var body: String
}

/// Decides which meetings to remind about; each start is reminded once.
public struct ReminderPlanner: Sendable {
    /// How long after the start a missed reminder still fires.
    public static let grace: TimeInterval = 5 * 60
    private var sent: Set<String> = []

    public init() {}

    public mutating func due(_ upcoming: [UpcomingMeeting], now: Date, minutesBefore: Int) -> [MeetingReminder] {
        guard minutesBefore > 0 else { return [] }
        let lead = TimeInterval(minutesBefore * 60)
        var out: [MeetingReminder] = []
        for m in upcoming {
            let key = "meeting-reminder-\(m.id)-\(Int(m.start.timeIntervalSince1970))"
            guard !sent.contains(key), now >= m.start.addingTimeInterval(-lead), now < m.start.addingTimeInterval(Self.grace) else { continue }
            sent.insert(key)
            let minutes = Int((m.start.timeIntervalSince(now) / 60).rounded())
            let when = minutes <= 0 ? "now" : "in \(minutes) min"
            out.append(MeetingReminder(identifier: key, roomId: m.roomId, title: "\(m.title) starts \(when)",
                                       body: m.title == m.roomName ? "Start the meeting when you're ready." : "In \(m.roomName). Start the meeting when you're ready."))
        }
        return out
    }
}

// MARK: write-back into a chosen calendar

/// Calendar access as the app sees it (EventKit's status, simplified).
public enum CalendarAuthorization: Equatable, Sendable {
    case notDetermined, denied, restricted, writeOnly, fullAccess

    public var canRead: Bool { self == .fullAccess }

    public var explanation: String {
        switch self {
        case .notDetermined: return "p2p.video hasn't asked for calendar access yet."
        case .denied: return "Calendar access is off. Turn it on in System Settings → Privacy & Security → Calendars."
        case .restricted: return "Calendar access is restricted on this Mac."
        case .writeOnly: return "p2p.video can add events but not read them. Allow full access in System Settings → Privacy & Security → Calendars."
        case .fullAccess: return "Calendar access allowed."
        }
    }
}

/// A calendar events can be written to.
public struct WritableCalendar: Equatable, Sendable, Identifiable {
    public var id: String
    public var title: String
    public var account: String
    public init(id: String, title: String, account: String) { self.id = id; self.title = title; self.account = account }
}

/// One event the app writes for a scheduled meeting.
public struct ScheduledEventDraft: Equatable, Sendable {
    public var meetingId: String
    public var sequence: Int
    public var title: String
    public var start: Date
    public var end: Date
    public var notes: String
    public var url: URL?
    public var calendarId: String
}

/// Which events the app wrote, per scheduled meeting (`calendar-sync.json`).
public struct CalendarSyncLedger: Codable, Equatable, Sendable {
    public struct Entry: Codable, Equatable, Sendable {
        public var eventIdentifier: String
        public var sequence: Int
        public var calendarId: String
        public var end: Date
        public init(eventIdentifier: String, sequence: Int, calendarId: String, end: Date) {
            self.eventIdentifier = eventIdentifier; self.sequence = sequence; self.calendarId = calendarId; self.end = end
        }
    }
    public var entries: [String: Entry] = [:]

    public init(entries: [String: Entry] = [:]) { self.entries = entries }

    public static func load(from file: URL) -> CalendarSyncLedger {
        guard let data = try? Data(contentsOf: file), let l = try? JSONDecoder().decode(CalendarSyncLedger.self, from: data) else { return CalendarSyncLedger() }
        return l
    }

    public func save(to file: URL) throws {
        let enc = JSONEncoder()
        enc.outputFormatting = [.prettyPrinted, .sortedKeys]
        try FileManager.default.createDirectory(at: file.deletingLastPathComponent(), withIntermediateDirectories: true)
        try enc.encode(self).write(to: file, options: [.atomic])
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
    }
}

public enum CalendarSyncAction: Equatable, Sendable {
    case create(ScheduledEventDraft)
    case update(eventIdentifier: String, ScheduledEventDraft)
    case remove(meetingId: String, eventIdentifier: String)
}

public enum CalendarSyncPlanner {
    /// Mirrors upcoming scheduled meetings into `calendarId`. New meetings are
    /// created, a higher SEQUENCE updates, cancelled (or deleted while still
    /// upcoming) meetings are removed, a calendar change moves them. Past
    /// meetings are left alone. Meetings without a guest link wait.
    public static func plan(meetings: [ScheduledMeetingInvite], ledger: CalendarSyncLedger, calendarId: String, now: Date) -> [CalendarSyncAction] {
        var actions: [CalendarSyncAction] = []
        var listed = Set<String>()
        for m in meetings {
            listed.insert(m.id)
            let entry = ledger.entries[m.id]
            if m.cancelled {
                if let e = entry { actions.append(.remove(meetingId: m.id, eventIdentifier: e.eventIdentifier)) }
                continue
            }
            guard m.endsAt > now, m.linkReady else { continue }
            let draft = ScheduledEventDraft(meetingId: m.id, sequence: m.sequence, title: m.title, start: m.startsAt, end: m.endsAt,
                                            notes: m.invite.text, url: m.invite.guestUrl.flatMap(URL.init(string:)), calendarId: calendarId)
            switch entry {
            case nil:
                actions.append(.create(draft))
            case let e? where e.calendarId != calendarId:
                actions.append(.remove(meetingId: m.id, eventIdentifier: e.eventIdentifier))
                actions.append(.create(draft))
            case let e? where e.sequence < m.sequence:
                actions.append(.update(eventIdentifier: e.eventIdentifier, draft))
            default:
                break
            }
        }
        for (id, e) in ledger.entries.sorted(by: { $0.key < $1.key }) where !listed.contains(id) && e.end > now {
            actions.append(.remove(meetingId: id, eventIdentifier: e.eventIdentifier))
        }
        return actions
    }
}

/// The calendar store the app talks to: EventKit in the app, fakes in tests.
@MainActor
public protocol CalendarProvider: AnyObject {
    var authorization: CalendarAuthorization { get }
    /// Asks the user for full calendar access (the macOS prompt). Only on an
    /// explicit user action.
    func requestFullAccess() async -> Bool
    func events(from: Date, to: Date) -> [CalendarEventSnapshot]
    func writableCalendars() -> [WritableCalendar]
    /// Creates or updates an event; returns its identifier.
    func save(_ draft: ScheduledEventDraft, existing: String?) throws -> String
    func remove(eventIdentifier: String) throws
}

public enum CalendarSyncRunner {
    /// Applies actions through the provider and returns the new ledger plus
    /// readable failures. Never runs without full access.
    @MainActor
    public static func apply(_ actions: [CalendarSyncAction], provider: CalendarProvider, ledger: CalendarSyncLedger) -> (CalendarSyncLedger, [String]) {
        guard provider.authorization == .fullAccess else { return (ledger, actions.isEmpty ? [] : ["No calendar access."]) }
        var next = ledger
        var failures: [String] = []
        for action in actions {
            do {
                switch action {
                case .create(let d):
                    let id = try provider.save(d, existing: nil)
                    next.entries[d.meetingId] = .init(eventIdentifier: id, sequence: d.sequence, calendarId: d.calendarId, end: d.end)
                case .update(let existing, let d):
                    let id = try provider.save(d, existing: existing)
                    next.entries[d.meetingId] = .init(eventIdentifier: id, sequence: d.sequence, calendarId: d.calendarId, end: d.end)
                case .remove(let meetingId, let eventId):
                    try provider.remove(eventIdentifier: eventId)
                    next.entries[meetingId] = nil
                }
            } catch {
                failures.append("\(error)")
            }
        }
        return (next, failures)
    }
}
