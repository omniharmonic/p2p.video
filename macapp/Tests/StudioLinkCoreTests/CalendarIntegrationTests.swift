import Foundation
import StudioLinkCore
import XCTest

// [SL-P6-1] calendar matching, upcoming merge, reminders, write-back.

private let t0 = Date(timeIntervalSince1970: 1_793_000_000) // a fixed "now"
private func at(_ minutes: Double) -> Date { t0.addingTimeInterval(minutes * 60) }

private let directory = DesktopCalendar(
    rooms: [.init(id: "room_1", name: "The Podcast", slug: "the-podcast"), .init(id: "room_2", name: "Weekly sync", slug: "weekly-sync")],
    guestOrigins: ["https://studio.tail1234.ts.net", "http://127.0.0.1:7443", "http://localhost:7443"],
    scheduled: [
        .init(id: "mtg_a", roomId: "room_1", title: "Episode 12", startsAt: at(30), endsAt: at(120), sequence: 1, uid: "abc@studiolink"),
        .init(id: "mtg_x", roomId: "room_2", title: "Dropped", startsAt: at(60), endsAt: at(90), status: "cancelled", sequence: 2, uid: "gone@studiolink"),
    ])

final class CalendarDecodingTests: XCTestCase {
    func testDecodesTheDaemonsDesktopCalendarJSON() throws {
        let json = """
        {"rooms":[{"id":"room_1","name":"The Podcast","slug":"the-podcast","live":true}],
         "guestOrigins":["https://studio.tail1234.ts.net","http://127.0.0.1:7443"],
         "scheduled":[{"id":"mtg_a","roomId":"room_1","title":"Episode 12","startsAt":"2026-10-30T17:00:00Z","endsAt":"2026-10-30T18:30:00.123456789Z","status":"scheduled","sequence":3,"uid":"u@studiolink"}]}
        """
        let c = try DesktopCalendar.decode(Data(json.utf8))
        XCTAssertEqual(c.rooms.first?.slug, "the-podcast")
        XCTAssertTrue(c.rooms[0].live)
        XCTAssertEqual(c.scheduled[0].sequence, 3)
        XCTAssertEqual(c.scheduled[0].endsAt.timeIntervalSince(c.scheduled[0].startsAt), 5400.123, accuracy: 0.01)
    }

    func testDecodesScheduleListWithInvite() throws {
        let json = """
        {"meetings":[{"id":"mtg_a","roomId":"room_1","title":"Episode 12","description":"","startsAt":"2026-10-30T17:00:00Z","endsAt":"2026-10-30T18:30:00Z",
          "durationMin":90,"timeZone":"UTC","guestEmails":[],"uid":"u","sequence":0,"status":"scheduled","createdAt":"2026-10-26T09:00:00.5Z","updatedAt":"2026-10-26T09:00:00.5Z",
          "roomName":"The Podcast","roomArchived":false,"roomLive":false,"linkReady":true,
          "invite":{"when":"x","method":"PUBLISH","guestUrl":"https://studio.tail1234.ts.net/join?room=the-podcast&token=s","remote":true,"text":"Episode 12\\nJoin","googleUrl":"g","outlookUrl":"o","icsPath":"/p"}}]}
        """
        let list = try ScheduledMeetingInvite.decodeList(Data(json.utf8))
        XCTAssertEqual(list.count, 1)
        XCTAssertEqual(list[0].invite.text, "Episode 12\nJoin")
        XCTAssertTrue(list[0].linkReady)
    }
}

final class RoomLinkMatcherTests: XCTestCase {
    func testMatchesOnlyThisHostsRoomLinks() {
        let ok = URL(string: "https://studio.tail1234.ts.net/join?room=the-podcast&token=secret")!
        XCTAssertEqual(RoomLinkMatcher.match(ok, in: directory), RoomLinkMatch(roomId: "room_1", roomName: "The Podcast", slug: "the-podcast"))
        XCTAssertEqual(RoomLinkMatcher.match(URL(string: "HTTPS://Studio.Tail1234.ts.net:443/join?token=x&room=weekly-sync")!, in: directory)?.roomId, "room_2")
        XCTAssertEqual(RoomLinkMatcher.match(URL(string: "http://localhost:7443/join?room=the-podcast&token=x")!, in: directory)?.roomId, "room_1")
        for bad in ["https://other.tail9.ts.net/join?room=the-podcast&token=x", // someone else's p2p.video
                    "https://studio.tail1234.ts.net/join?room=unknown&token=x",
                    "https://studio.tail1234.ts.net/?room=the-podcast",
                    "http://127.0.0.1:9999/join?room=the-podcast",
                    "https://meet.google.com/abc-defg-hij"] {
            XCTAssertNil(RoomLinkMatcher.match(URL(string: bad)!, in: directory), bad)
        }
    }

    func testFindsLinksInLocationAndNotes() {
        let notes = "Agenda.\n\nJoin the call: https://studio.tail1234.ts.net/join?room=weekly-sync&token=abc.\nThanks!"
        XCTAssertEqual(RoomLinkMatcher.urls(in: notes).map(\.absoluteString), ["https://studio.tail1234.ts.net/join?room=weekly-sync&token=abc"])
        let e = CalendarEventSnapshot(identifier: "e1", title: "Sync", start: at(10), end: at(40), location: "Zoom? no: (https://studio.tail1234.ts.net/join?room=the-podcast&token=t)")
        XCTAssertEqual(RoomLinkMatcher.match(event: e, in: directory)?.roomId, "room_1")
        let viaNotes = CalendarEventSnapshot(identifier: "e2", title: "Sync", start: at(10), end: at(40), notes: notes)
        XCTAssertEqual(RoomLinkMatcher.match(event: viaNotes, in: directory)?.roomId, "room_2")
        let none = CalendarEventSnapshot(identifier: "e3", title: "Lunch", start: at(10), end: at(40), notes: "https://example.com/menu")
        XCTAssertNil(RoomLinkMatcher.match(event: none, in: directory))
    }
}

final class UpcomingPlannerTests: XCTestCase {
    func testMergesScheduledAndCalendarEventsWithoutDuplicates() {
        let link = URL(string: "https://studio.tail1234.ts.net/join?room=the-podcast&token=t")!
        let events = [
            // The same scheduled meeting imported from the .ics (same UID): shown once.
            CalendarEventSnapshot(identifier: "ek1", externalIdentifier: "abc@studiolink", calendarTitle: "Work", title: "Episode 12", start: at(30), end: at(120), url: link),
            // Added via the Google template link (no UID), same room and start: shown once.
            CalendarEventSnapshot(identifier: "ek2", calendarTitle: "Google", title: "Episode 12 (Google)", start: at(30.5), end: at(120), location: link.absoluteString),
            // A different meeting in a room, from someone's own calendar entry.
            CalendarEventSnapshot(identifier: "ek3", calendarTitle: "Home", title: "", start: at(10), end: at(40), notes: "https://studio.tail1234.ts.net/join?room=weekly-sync&token=x"),
            // The cancelled scheduled meeting still in a calendar: hidden.
            CalendarEventSnapshot(identifier: "ek4", externalIdentifier: "gone@studiolink", title: "Dropped", start: at(60), end: at(90), url: URL(string: "https://studio.tail1234.ts.net/join?room=weekly-sync&token=x")!),
            // Over, and not a p2p.video event.
            CalendarEventSnapshot(identifier: "ek5", title: "Old", start: at(-90), end: at(-30), url: link),
            CalendarEventSnapshot(identifier: "ek6", title: "Dentist", start: at(20), end: at(50)),
        ]
        let up = UpcomingPlanner.merge(events: events, directory: directory, now: t0)
        XCTAssertEqual(up.map(\.title), ["Weekly sync", "Episode 12"])
        XCTAssertEqual(up[0].source, .calendar("Home"))
        XCTAssertEqual(up[0].roomName, "Weekly sync")
        XCTAssertEqual(up[1].source, .scheduled)
        XCTAssertEqual(up[1].scheduledMeetingId, "mtg_a")
        // Beyond the horizon: not listed.
        XCTAssertTrue(UpcomingPlanner.merge(events: [], directory: directory, now: t0, horizon: 10 * 60).isEmpty)
    }
}

final class ReminderPlannerTests: XCTestCase {
    func testRemindsOncePerStartWithinTheLeadTime() {
        var planner = ReminderPlanner()
        let m = UpcomingMeeting(id: "scheduled:mtg_a", roomId: "room_1", roomName: "The Podcast", title: "Episode 12", start: at(30), end: at(120), source: .scheduled, scheduledMeetingId: "mtg_a")
        XCTAssertTrue(planner.due([m], now: t0, minutesBefore: 5).isEmpty, "too early")
        let r = planner.due([m], now: at(26), minutesBefore: 5)
        XCTAssertEqual(r.count, 1)
        XCTAssertEqual(r[0].roomId, "room_1")
        XCTAssertEqual(r[0].title, "Episode 12 starts in 4 min")
        XCTAssertTrue(r[0].body.contains("The Podcast"))
        XCTAssertTrue(planner.due([m], now: at(28), minutesBefore: 5).isEmpty, "once")
        // Rescheduled: a new start is a new reminder.
        var moved = m; moved.start = at(60); moved.end = at(150)
        XCTAssertEqual(planner.due([moved], now: at(56), minutesBefore: 5).count, 1)
        // Missed by more than the grace period, or reminders off: nothing.
        var fresh = ReminderPlanner()
        XCTAssertTrue(fresh.due([m], now: at(36), minutesBefore: 5).isEmpty)
        XCTAssertTrue(fresh.due([m], now: at(29), minutesBefore: 0).isEmpty)
        XCTAssertEqual(fresh.due([m], now: at(31), minutesBefore: 5).first?.title, "Episode 12 starts now")
    }
}

@MainActor
private final class FakeCalendar: CalendarProvider {
    var authorization: CalendarAuthorization = .notDetermined
    var grant = true
    var requests = 0
    var stored: [String: ScheduledEventDraft] = [:]
    var failRemove = false
    private var next = 0

    func requestFullAccess() async -> Bool {
        requests += 1
        authorization = grant ? .fullAccess : .denied
        return grant
    }
    func events(from: Date, to: Date) -> [CalendarEventSnapshot] {
        guard authorization.canRead else { return [] }
        return stored.map { CalendarEventSnapshot(identifier: $0.key, title: $0.value.title, start: $0.value.start, end: $0.value.end, url: $0.value.url) }
    }
    func writableCalendars() -> [WritableCalendar] { [WritableCalendar(id: "cal_home", title: "Home", account: "iCloud")] }
    func save(_ draft: ScheduledEventDraft, existing: String?) throws -> String {
        let id = existing ?? { next += 1; return "ev\(next)" }()
        stored[id] = draft
        return id
    }
    func remove(eventIdentifier: String) throws {
        if failRemove { throw CocoaError(.fileWriteUnknown) }
        stored[eventIdentifier] = nil
    }
}

final class CalendarSyncTests: XCTestCase {
    private func meeting(_ id: String, seq: Int = 0, status: String = "scheduled", start: Double = 60, linkReady: Bool = true) -> ScheduledMeetingInvite {
        ScheduledMeetingInvite(id: id, roomId: "room_1", title: "T \(id) \(seq)", startsAt: at(start), endsAt: at(start + 60), status: status, sequence: seq, linkReady: linkReady,
                               invite: .init(text: "Invite \(id)", guestUrl: "https://studio.tail1234.ts.net/join?room=the-podcast&token=s"))
    }

    func testPlansCreateUpdateMoveAndRemove() {
        var ledger = CalendarSyncLedger()
        var actions = CalendarSyncPlanner.plan(meetings: [meeting("a"), meeting("b", linkReady: false)], ledger: ledger, calendarId: "cal", now: t0)
        XCTAssertEqual(actions.count, 1, "no guest link yet: wait")
        guard case .create(let d) = actions[0] else { return XCTFail("\(actions)") }
        XCTAssertEqual(d.notes, "Invite a")
        XCTAssertEqual(d.url?.host, "studio.tail1234.ts.net")
        ledger.entries["a"] = .init(eventIdentifier: "ev1", sequence: 0, calendarId: "cal", end: at(120))
        XCTAssertTrue(CalendarSyncPlanner.plan(meetings: [meeting("a")], ledger: ledger, calendarId: "cal", now: t0).isEmpty, "unchanged")
        actions = CalendarSyncPlanner.plan(meetings: [meeting("a", seq: 1)], ledger: ledger, calendarId: "cal", now: t0)
        XCTAssertEqual(actions.count, 1)
        if case .update(let ev, let d) = actions[0] { XCTAssertEqual(ev, "ev1"); XCTAssertEqual(d.sequence, 1) } else { XCTFail() }
        actions = CalendarSyncPlanner.plan(meetings: [meeting("a")], ledger: ledger, calendarId: "other", now: t0)
        XCTAssertEqual(actions.count, 2, "moved calendars: remove then create")
        actions = CalendarSyncPlanner.plan(meetings: [meeting("a", seq: 2, status: "cancelled")], ledger: ledger, calendarId: "cal", now: t0)
        XCTAssertEqual(actions, [.remove(meetingId: "a", eventIdentifier: "ev1")])
        // Deleted while upcoming: removed. Past and no longer listed: left alone.
        XCTAssertEqual(CalendarSyncPlanner.plan(meetings: [], ledger: ledger, calendarId: "cal", now: t0), [.remove(meetingId: "a", eventIdentifier: "ev1")])
        XCTAssertTrue(CalendarSyncPlanner.plan(meetings: [], ledger: ledger, calendarId: "cal", now: at(500)).isEmpty)
    }

    @MainActor
    func testRunnerWritesThroughTheProviderOnlyWithAccess() async throws {
        let cal = FakeCalendar()
        let plan = CalendarSyncPlanner.plan(meetings: [meeting("a")], ledger: CalendarSyncLedger(), calendarId: "cal_home", now: t0)
        var (ledger, failures) = CalendarSyncRunner.apply(plan, provider: cal, ledger: CalendarSyncLedger())
        XCTAssertTrue(cal.stored.isEmpty, "never writes without access")
        XCTAssertEqual(failures, ["No calendar access."])
        XCTAssertEqual(cal.requests, 0, "the runner never prompts")

        cal.grant = false
        let granted = await cal.requestFullAccess()
        XCTAssertFalse(granted)
        XCTAssertEqual(cal.authorization, .denied)
        XCTAssertTrue(cal.authorization.explanation.contains("System Settings"))
        XCTAssertTrue(cal.events(from: t0, to: at(600)).isEmpty, "denied degrades to nothing")

        cal.grant = true
        _ = await cal.requestFullAccess()
        (ledger, failures) = CalendarSyncRunner.apply(plan, provider: cal, ledger: CalendarSyncLedger())
        XCTAssertEqual(failures, [])
        XCTAssertEqual(ledger.entries["a"]?.eventIdentifier, "ev1")
        XCTAssertEqual(cal.stored["ev1"]?.title, "T a 0")

        // The written event is recognized as the scheduled meeting, not listed twice.
        let dir = DesktopCalendar(rooms: directory.rooms, guestOrigins: directory.guestOrigins,
                                  scheduled: [.init(id: "a", roomId: "room_1", title: "T a 0", startsAt: at(60), endsAt: at(120), uid: "a@studiolink")])
        XCTAssertEqual(UpcomingPlanner.merge(events: cal.events(from: t0, to: at(600)), directory: dir, now: t0).count, 1)

        let update = CalendarSyncPlanner.plan(meetings: [meeting("a", seq: 1)], ledger: ledger, calendarId: "cal_home", now: t0)
        (ledger, _) = CalendarSyncRunner.apply(update, provider: cal, ledger: ledger)
        XCTAssertEqual(cal.stored["ev1"]?.title, "T a 1")
        XCTAssertEqual(ledger.entries["a"]?.sequence, 1)

        cal.failRemove = true
        let cancel = CalendarSyncPlanner.plan(meetings: [meeting("a", seq: 2, status: "cancelled")], ledger: ledger, calendarId: "cal_home", now: t0)
        (ledger, failures) = CalendarSyncRunner.apply(cancel, provider: cal, ledger: ledger)
        XCTAssertEqual(failures.count, 1)
        XCTAssertNotNil(ledger.entries["a"], "a failed removal is retried next time")
        cal.failRemove = false
        (ledger, failures) = CalendarSyncRunner.apply(cancel, provider: cal, ledger: ledger)
        XCTAssertTrue(failures.isEmpty)
        XCTAssertNil(ledger.entries["a"])
        XCTAssertTrue(cal.stored.isEmpty)
    }

    func testLedgerRoundTripsWithOwnerOnlyPermissions() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: dir) }
        let file = dir.appendingPathComponent("calendar-sync.json")
        XCTAssertEqual(CalendarSyncLedger.load(from: file), CalendarSyncLedger())
        let l = CalendarSyncLedger(entries: ["a": .init(eventIdentifier: "ev1", sequence: 2, calendarId: "cal", end: t0)])
        try l.save(to: file)
        XCTAssertEqual(CalendarSyncLedger.load(from: file), l)
        let perms = try FileManager.default.attributesOfItem(atPath: file.path)[.posixPermissions] as? Int
        XCTAssertEqual(perms, 0o600)
    }

    func testCalendarSettingsDefaultOffAndDecodeTolerantly() throws {
        let d = DesktopSettings()
        XCTAssertFalse(d.calendarEnabled)
        XCTAssertFalse(d.calendarWriteEnabled)
        XCTAssertEqual(d.calendarReminderMinutes, 5)
        let old = try JSONDecoder().decode(DesktopSettings.self, from: Data(#"{"guestAccess":"off"}"#.utf8))
        XCTAssertFalse(old.calendarEnabled, "settings from older apps keep calendar access off")
        let clamped = try JSONDecoder().decode(DesktopSettings.self, from: Data(#"{"calendarEnabled":true,"calendarReminderMinutes":9999,"calendarWriteCalendarID":"cal"}"#.utf8))
        XCTAssertTrue(clamped.calendarEnabled)
        XCTAssertEqual(clamped.calendarReminderMinutes, 120)
        XCTAssertEqual(clamped.calendarWriteCalendarID, "cal")
    }
}
