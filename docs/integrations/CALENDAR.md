# Calendar integration (SL-P6-1, ADR-018)

p2p.video works with Google Calendar, iCloud, Outlook and anything else that
reads iCalendar, without a calendar API, an OAuth app registration or any hosted
service. There are four pieces:

1. **Scheduling in p2p.video.** The host schedules a meeting in a room (title,
   start, length, agenda, optional guest emails, optional organizer email). Home
   → **Upcoming** lists it with Start, Invite, Edit, Cancel and Delete.
2. **Invites any calendar accepts.** Each meeting has a downloadable `.ics`,
   an "Add to Google Calendar" template link, an Outlook on the web link, an
   "Email guests" `mailto:` link and copy-paste invite text. They are built
   from the room's guest link. Nothing is emailed for the host.
3. **A calendar feed** for the host: an iCalendar subscription of upcoming
   meetings, served to this Mac and to tailnet devices only, behind a
   rotatable secret. It is never served through Funnel.
4. **The Mac app reads macOS calendars** (EventKit, so it sees the Google,
   iCloud and Exchange accounts already added to macOS). It shows upcoming
   meetings that hold one of this host's room links, sends a reminder with a
   Start meeting action, and can optionally write scheduled meetings into a
   calendar the user picks. Every feature is off until the user turns it on.

Out of scope: detecting Google Meet, Zoom or Teams links in calendar events,
two-way sync with a calendar service, recurring meetings, and RSVP tracking.
These are listed under Future below.

## Scheduling (daemon)

Table `scheduled_meetings` (migration 6): `id`, `room_id` (deleted with the
room), `title`, `description`, `starts_at` (fixed-width UTC text so it sorts),
`duration_min`, `time_zone` (IANA, for display only), `guest_emails` (JSON),
`organizer_email`, `uid` (stable iCalendar UID, `<128-bit hex>@studiolink`),
`sequence`, `status` (`scheduled` | `cancelled`), timestamps.

| Route (admin credential: launch cookie or desktop token; never Funnel) | |
|---|---|
| `GET /api/v1/schedule` | Meetings that haven't ended, cancelled ones included, each with its invite |
| `POST /api/v1/schedule` | `{roomId, startsAt (RFC 3339), durationMin?, timeZone?, title?, description?, guestEmails?, organizerEmail?}` |
| `GET/PATCH/DELETE /api/v1/schedule/{id}` | A PATCH that changes anything guests see bumps `sequence` |
| `POST /api/v1/schedule/{id}/cancel` | `status=cancelled`, `sequence+1` (idempotent) |
| `GET /api/v1/schedule/{id}/invite.ics` | `text/calendar` attachment; 409 while the room's guest link is off (a cancellation always downloads) |

Validation: title 1–120 characters (room name by default), agenda ≤ 4000
characters (CR and control characters dropped), length 5–720 minutes, start
from yesterday up to two years ahead, a known IANA zone, ≤ 50 guest addresses
matching a conservative `local@domain.tld` pattern (no `?`, `&`, quotes or
spaces, so an address can't add `bcc=` to a `mailto:` link), archived rooms
refused. A meeting is started by opening its room, exactly like Rooms → Start.

## Invites

`host/internal/calendar` renders everything. Golden tests cover the output in
`host/internal/calendar/testdata/*.ics`. The same files were parsed with the
python `icalendar` 6.3.2 package (in a scratch virtualenv, not a dependency).

- **iCalendar (RFC 5545)**: CRLF line endings; lines folded at 75 octets
  (continuations start with one space, and a UTF-8 sequence is never split);
  TEXT escaping of `\`, `;`, `,` and newlines; control and bidi characters are
  dropped. Times are UTC (`DTSTART:20261030T170000Z`), so no `VTIMEZONE` is
  needed. Each event has `UID`, `SEQUENCE`, `DTSTAMP`/`LAST-MODIFIED` (the
  meeting's update time), `SUMMARY`, `DESCRIPTION`, `LOCATION` and `URL` (the
  guest link), `STATUS`, `TRANSP:OPAQUE` and a 10-minute `VALARM`.
- **Method**: `PUBLISH` (an event to add, no attendees) by default. It is
  `REQUEST` with `ORGANIZER` and `ATTENDEE;RSVP=TRUE` when the host gave their
  own email and at least one guest email, so mail clients treat it as an
  invitation and RSVPs go to the host's mailbox. `CANCEL` with
  `STATUS:CANCELLED` and a higher `SEQUENCE` for a cancelled meeting. Calendars
  replace an event only when `SEQUENCE` goes up, so every edit guests can see
  bumps it.
- **Description**: "Join the call: <guest link>", the agenda, then guest tips:
  use Chrome on a laptop or desktop, wear headphones (wired are best), a quiet
  room with light in front, the device check before joining (no account or
  app), and keep the tab open until the recording uploads.
- **Google Calendar**:
  `https://calendar.google.com/calendar/render?action=TEMPLATE&text=…&dates=<UTC>/<UTC>&details=…&location=<guest link>&add=<guests>&ctz=<zone>`.
  It's a plain URL that opens in the host's browser.
- **Outlook on the web**:
  `https://outlook.live.com/calendar/0/deeplink/compose?path=/calendar/action/compose&rru=addevent&…`.
  For work accounts, the same query works on `outlook.office.com`.
- **Email guests**: `mailto:<guests>?subject=Invitation: …&body=<invite text>`
  opens the host's own mail app. Attach the `.ics` there if you want.

The guest link is the only credential in an invite. Host URLs, host tokens,
launch tokens and dashboard addresses never appear (tested in Go and in the
browser). If the room has no remote guest link yet (no Tailscale Serve or
Funnel), the share dialog warns that the link only works on this Mac. After
**rotating a room's guest link**, the feed and the dashboard pick up the new
link right away, but invites already sent hold the old link. Send them again
(download the `.ics` again; `SEQUENCE` is unchanged, so calendars that ignore
same-sequence updates need the new event re-imported or the link pasted).

## Calendar feed

Home → Upcoming → **Feed** → Turn on feed. The feed secret is derived like a
room link: `HMAC-SHA256(daemon key, "studiolink/calendar-feed/v1" ‖ nonce)`,
256 bits. The database keeps only the nonce and the SHA-256 of the secret
(table `calendar_feed`). Replace address rotates the nonce, so old
subscriptions stop updating at once. Turn off clears it.

- Address: `<origin>/api/v1/calendar/feed.ics?key=<secret>`. Calendar apps
  can't send headers, so the key is in the URL. The origin is the Tailscale
  Serve address when there is one (`webcal://` link offered), and the dialog
  also gives the `http://127.0.0.1:<port>` address for this Mac.
- **Who can fetch it**: a direct loopback request (this Mac), or a private
  Tailscale Serve request whose `X-Forwarded-For` is a tailnet address
  (100.64.0.0/10, fd7a:115c:a1e0::/48). A request carrying
  `Tailscale-Funnel-Request` is refused with 403 even with the right key, and
  so is any other position (LAN, public proxy). It is covered by
  `TestCalendarFeedTailnetOnlyRotatableNeverFunnel` and the Funnel guardrail
  test.
- Contents: meetings not cancelled that end within the last 30 days or later,
  no `METHOD`, `X-WR-CALNAME:p2p.video meetings`, a 15-minute
  `REFRESH-INTERVAL`, and no organizer or attendees. `Cache-Control: no-store`.
- **Google Calendar can't use it**: "From URL" subscriptions are fetched by
  Google's servers, which can't reach a tailnet. For Google, use "Add to
  Google Calendar" per meeting, or the Mac app's write-back into a Google
  calendar added to macOS.

## Mac app (EventKit)

Pure logic is in `StudioLinkCore/CalendarIntegration.swift`, and
`CalendarIntegrationTests` tests it with a fake `CalendarProvider`. The EventKit
glue is in `Sources/StudioLink/CalendarMonitor.swift`.

- **Matching data**: `GET /api/v1/desktop/calendar` uses direct loopback and the
  `.desktop_token`, like `/desktop/state`, and is refused through Funnel, from a
  browser Origin or behind Serve headers. It returns rooms (`id`, `name`,
  `slug`, `live`), `guestOrigins` (Serve/public URL, `127.0.0.1:<port>`,
  `localhost:<port>`) and scheduled meeting times and UIDs. It carries no link
  secrets and no guest emails.
- **Matching rule**: an event matches when its URL, location or notes contain
  `<one of guestOrigins>/join?room=<slug>` for a known room. Origins are
  compared case-insensitively, with default ports dropped. The token in the
  link is never read or kept. Another host's p2p.video link, an unknown slug
  or a Meet link doesn't match.
- **Upcoming**: matched events in the next 7 days are merged with the daemon's
  scheduled meetings. A calendar copy of a scheduled meeting (same UID, which
  `.ics` imports and CalDAV keep, or the same room starting within a minute,
  which covers the Google template link) is shown once. Events for cancelled
  meetings are hidden. Upcoming meetings appear in the menu bar under
  **Upcoming** and in Settings → Calendar, each with Start.
- **Reminders**: N minutes before the start (Settings: off, 1, 5, 10, 15 or
  30; default 5), once per meeting start. If the app was asleep, a reminder
  still goes out up to 5 minutes after the start. The notification has a
  **Start meeting** action, and a click opens the room's home with the room
  preselected, as Rooms → Open does.
- **Write-back (off by default)**: Settings → Calendar → "Add meetings I
  schedule in p2p.video to" + a calendar picker (writable calendars only). New
  meetings are created; a higher `SEQUENCE` updates the event; cancelling
  removes it; a meeting deleted while still upcoming removes it; past events
  are left alone. A meeting waits while its room has no guest link. The app
  keeps a ledger, `<data dir>/calendar-sync.json` (0600, meeting → event
  identifier, sequence, calendar). An event the user deleted by hand is
  recreated on the next change. The event's notes are the invite text and its
  URL/location is the guest link.
- **Permission**: macOS 14+ `requestFullAccessToEvents` is called only when the
  user turns on a calendar toggle. Full access is needed because matching
  reads events and write-back must find its own events again to update them.
  `NSCalendarsFullAccessUsageDescription`/`NSCalendarsUsageDescription` are in
  the bundle's Info.plist, and the hardened runtime gets
  `com.apple.security.personal-information.calendars`. Without a grant
  (denied, restricted or write-only), reading returns nothing and Settings
  explains where to allow it. Scheduled meetings from the daemon are still
  listed and reminded. A bare `swift run` binary has no usage string and
  doesn't prompt.

## Verified (2026-10-26, synthetic)

- Go: ICS golden files (escaping, folding at 75 octets incl. a 4-byte emoji on
  the boundary, CRLF, UTC, PUBLISH/REQUEST/CANCEL, SEQUENCE), template links,
  store migration 6 on the real version-5 fixture `pre_calendar.db`,
  lifecycle/validation/authorization tests, feed position tests, and the
  Funnel guardrail on every new route.
- Web: `schedule.test.mjs`; `calendar.browser.mjs` (schedule with a new room →
  Upcoming → copy invite → `.ics` REQUEST → template links → feed on this Mac,
  refused with the Funnel header → edit raises `SEQUENCE` → Start opens the
  room → cancel produces `CANCEL` and leaves the feed → remove), no overflow at
  1280 and 390 px. Every downloaded `.ics` was also parsed by python
  `icalendar` (`ICAL_PYTHON`).
- Swift: matching, merge and dedupe, reminders, write-back planning and
  running, ledger permissions and settings defaults (fake provider).

## Not verified

- A real macOS calendar-permission prompt and real EventKit reads and writes.
  The prompt can't be granted unattended, and tests never touch real
  calendars.
- Importing the `.ics` into real Google Calendar, Apple Calendar and Outlook,
  including how each client handles `METHOD:CANCEL` without an organizer and
  same-`SEQUENCE` re-imports.
- macOS Calendar subscribing to the feed over a real Tailscale Serve URL.

## Future

- Recognize Google Meet, Zoom and Teams links in calendar events, for example
  to offer "move this meeting to p2p.video". This is out of scope for SL-P6-1.
- Recurring meetings (`RRULE`) for weekly shows.
- Bump `SEQUENCE` for upcoming meetings when a room's guest link is rotated.
