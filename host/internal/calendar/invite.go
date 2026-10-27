package calendar

import (
	"net/url"
	"strings"
	"time"

	// Invite times are written in the host's IANA time zone; embed the
	// database so that never depends on the machine's zoneinfo.
	_ "time/tzdata"
)

// Meeting is a scheduled meeting as invites see it. GuestURL is the room's
// guest link (it carries the link secret); it is the only credential an
// invite ever contains. Empty means the room has no working guest link.
type Meeting struct {
	UID         string
	Sequence    int
	Title       string
	Description string
	Start       time.Time
	Duration    time.Duration
	// TimeZone is the IANA zone the host scheduled in; it only affects the
	// human-readable time (calendar data is UTC). Empty or unknown is UTC.
	TimeZone    string
	GuestURL    string
	GuestEmails []string
	Organizer   *Person
	Cancelled   bool
	Created     time.Time
	Updated     time.Time
}

// End is the meeting's end time.
func (m *Meeting) End() time.Time { return m.Start.Add(m.Duration) }

// ReminderMinutes is the display alarm on every invite.
const ReminderMinutes = 10

// GuestTips are the "before you join" lines in every invite.
var GuestTips = []string{
	"Use Google Chrome on a laptop or desktop computer.",
	"Wear headphones (wired are best) so your microphone records only your voice.",
	"Sit somewhere quiet, with light in front of you rather than behind.",
	"You will check your camera and microphone before you join. No account or app needed.",
	"After the call, keep the tab open until your recording finishes uploading.",
}

// Body is the invite's description: how to join, the agenda, guest tips.
// It is the ICS DESCRIPTION and the template links' details.
func Body(m *Meeting) string {
	var b strings.Builder
	if m.Cancelled {
		b.WriteString("This meeting was cancelled.\n\n")
	}
	if m.GuestURL != "" {
		b.WriteString("Join the call: " + m.GuestURL + "\n")
	} else {
		b.WriteString("The host will send you the link to join.\n")
	}
	if d := strings.TrimSpace(m.Description); d != "" {
		b.WriteString("\n" + d + "\n")
	}
	b.WriteString("\nBefore you join:\n")
	for _, t := range GuestTips {
		b.WriteString("- " + t + "\n")
	}
	b.WriteString("\nHosted with p2p.video.")
	return b.String()
}

// Location resolves the meeting's IANA zone, falling back to UTC.
func Location(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// When is the human-readable time range in the host's zone, e.g.
// "Friday, October 30, 2026, 10:00 AM – 11:00 AM PDT".
func When(m *Meeting) string {
	loc := Location(m.TimeZone)
	start, end := m.Start.In(loc), m.End().In(loc)
	s := start.Format("Monday, January 2, 2006, 3:04 PM")
	if start.YearDay() == end.YearDay() && start.Year() == end.Year() {
		s += " – " + end.Format("3:04 PM")
	} else {
		s += " – " + end.Format("Monday, January 2, 3:04 PM")
	}
	abbr := end.Format("MST")
	if strings.HasPrefix(abbr, "+") || strings.HasPrefix(abbr, "-") {
		abbr = "UTC" + abbr
	}
	s += " " + abbr
	if loc != time.UTC && loc.String() != "UTC" {
		s += " (" + loc.String() + ")"
	}
	return s
}

// Text is the copy-and-paste invite: title, time, then the body.
func Text(m *Meeting) string {
	title := m.Title
	if m.Cancelled {
		title = "Cancelled: " + title
	}
	return title + "\n" + When(m) + "\n\n" + Body(m)
}

// Event maps a meeting to a VEVENT. LOCATION and URL are the guest link.
func (m *Meeting) Event() Event {
	e := Event{
		UID:          m.UID,
		Sequence:     m.Sequence,
		Stamp:        m.Updated,
		Created:      m.Created,
		LastModified: m.Updated,
		Start:        m.Start,
		End:          m.End(),
		Summary:      m.Title,
		Description:  Body(m),
		Location:     m.GuestURL,
		URL:          m.GuestURL,
		Cancelled:    m.Cancelled,
		Organizer:    m.Organizer,
		AlarmMinutes: ReminderMinutes,
	}
	for _, addr := range m.GuestEmails {
		e.Attendees = append(e.Attendees, Person{Email: addr})
	}
	return e
}

// InviteMethod is CANCEL for a cancelled meeting; REQUEST when there is an
// organizer address and at least one guest (a real invitation, RSVPs go to
// the organizer); otherwise PUBLISH (a plain event to add), which needs no
// ORGANIZER.
func InviteMethod(m *Meeting) Method {
	switch {
	case m.Cancelled:
		return MethodCancel
	case m.Organizer != nil && m.Organizer.Email != "" && len(m.GuestEmails) > 0:
		return MethodRequest
	default:
		return MethodPublish
	}
}

// InviteICS is the downloadable .ics for one meeting.
func InviteICS(m *Meeting) []byte {
	e := m.Event()
	method := InviteMethod(m)
	if method == MethodPublish {
		// PUBLISH carries no attendee list (RFC 5546 §3.2.1).
		e.Attendees = nil
	}
	c := Calendar{Method: method, Events: []Event{e}}
	return c.Encode()
}

// FeedICS is the host's subscription feed: no METHOD, a calendar name and
// a refresh hint. Organizer and attendees are left out: the feed is the
// host's own view, not an invitation.
func FeedICS(name string, meetings []Meeting) []byte {
	c := Calendar{Name: name, Refresh: 15 * time.Minute}
	for i := range meetings {
		e := meetings[i].Event()
		e.Organizer, e.Attendees = nil, nil
		c.Events = append(c.Events, e)
	}
	return c.Encode()
}

// encode encodes like url.Values but with %20 for spaces, which every
// calendar web app decodes the same way.
func encode(v url.Values) string { return strings.ReplaceAll(v.Encode(), "+", "%20") }

// GoogleURL is a Google Calendar "create event" template link: a plain URL,
// no API, no OAuth. Opening it sends these details to Google in the host's
// own browser.
func GoogleURL(m *Meeting) string {
	v := url.Values{
		"action":  {"TEMPLATE"},
		"text":    {m.Title},
		"dates":   {UTCStamp(m.Start) + "/" + UTCStamp(m.End())},
		"details": {Body(m)},
	}
	if m.GuestURL != "" {
		v.Set("location", m.GuestURL)
	}
	if len(m.GuestEmails) > 0 {
		v.Set("add", strings.Join(m.GuestEmails, ","))
	}
	if tz := Location(m.TimeZone).String(); tz != "UTC" {
		v.Set("ctz", tz)
	}
	return "https://calendar.google.com/calendar/render?" + encode(v)
}

// OutlookURL is an Outlook on the web compose deeplink (Outlook.com; the
// same query works on outlook.office.com for work accounts).
func OutlookURL(m *Meeting) string {
	v := url.Values{
		"path":    {"/calendar/action/compose"},
		"rru":     {"addevent"},
		"subject": {m.Title},
		"startdt": {m.Start.UTC().Format("2006-01-02T15:04:05Z")},
		"enddt":   {m.End().UTC().Format("2006-01-02T15:04:05Z")},
		"body":    {Body(m)},
	}
	if m.GuestURL != "" {
		v.Set("location", m.GuestURL)
	}
	if len(m.GuestEmails) > 0 {
		v.Set("to", strings.Join(m.GuestEmails, ","))
	}
	return "https://outlook.live.com/calendar/0/deeplink/compose?" + encode(v)
}

// MailtoURL opens the host's own mail client with the invite text addressed
// to the guests (no p2p.video mail service exists). Empty without guests.
func MailtoURL(m *Meeting) string {
	if len(m.GuestEmails) == 0 {
		return ""
	}
	subject := "Invitation: " + m.Title
	if m.Cancelled {
		subject = "Cancelled: " + m.Title
	}
	q := encode(url.Values{"subject": {subject}, "body": {strings.ReplaceAll(Text(m), "\n", "\r\n")}})
	return "mailto:" + strings.Join(m.GuestEmails, ",") + "?" + q
}

// Filename is a safe download name for a meeting's .ics.
func Filename(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r < 128 && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 60 {
			break
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "meeting"
	}
	return name + ".ics"
}
