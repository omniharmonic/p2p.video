package calendar

import (
	"bytes"
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var update = flag.Bool("update", false, "rewrite golden files")

const guestLink = "https://studio.tail1234.ts.net/join?room=the-podcast&token=0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fixture() Meeting {
	created := time.Date(2026, 10, 26, 9, 15, 0, 0, time.UTC)
	return Meeting{
		UID:         "mtg_4f1c2a9be0d3@studiolink",
		Sequence:    0,
		Title:       "Épisode 12; Ada, Grace & the \\backslash 🎙️",
		Description: "Agenda:\n1. Cold open, 5 min; intros\n2. Main topic — compilers, history & “why it matters”\n3. Wrap-up\n\nWe record locally: please use headphones.",
		Start:       time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC),
		Duration:    90 * time.Minute,
		TimeZone:    "America/Los_Angeles",
		GuestURL:    guestLink,
		GuestEmails: []string{"ada@example.com", "grace@example.org"},
		Created:     created,
		Updated:     created,
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update): %v", name, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// checkWellFormed is the strict hand check: CRLF only, every physical line
// at most 75 octets, continuation lines start with one space, no UTF-8
// sequence split across a fold, balanced BEGIN/END.
func checkWellFormed(t *testing.T, raw []byte) []string {
	t.Helper()
	s := string(raw)
	if !strings.HasSuffix(s, "\r\n") {
		t.Fatal("must end with CRLF")
	}
	if strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\n") || strings.Contains(strings.ReplaceAll(s, "\r\n", ""), "\r") {
		t.Fatal("bare CR or LF")
	}
	physical := strings.Split(strings.TrimSuffix(s, "\r\n"), "\r\n")
	var logical []string
	for i, l := range physical {
		if len(l) > 75 {
			t.Errorf("line %d is %d octets: %q", i+1, len(l), l)
		}
		if !utf8.ValidString(l) {
			t.Errorf("line %d splits a UTF-8 sequence: %q", i+1, l)
		}
		if strings.HasPrefix(l, " ") {
			if len(logical) == 0 {
				t.Fatal("continuation before first line")
			}
			logical[len(logical)-1] += l[1:]
			continue
		}
		logical = append(logical, l)
	}
	depth := 0
	for _, l := range logical {
		if strings.HasPrefix(l, "BEGIN:") {
			depth++
		}
		if strings.HasPrefix(l, "END:") {
			depth--
		}
	}
	if depth != 0 || logical[0] != "BEGIN:VCALENDAR" || logical[len(logical)-1] != "END:VCALENDAR" {
		t.Fatalf("unbalanced calendar: %v", logical)
	}
	return logical
}

func prop(lines []string, name string) (string, bool) {
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, ":"); ok && (k == name || strings.HasPrefix(k, name+";")) {
			return v, true
		}
	}
	return "", false
}

func unescape(s string) string {
	r := strings.NewReplacer(`\\`, `\`, `\;`, `;`, `\,`, `,`, `\n`, "\n", `\N`, "\n")
	return r.Replace(s)
}

func TestInvitePublishGolden(t *testing.T) {
	m := fixture()
	raw := InviteICS(&m)
	golden(t, "invite-publish.ics", raw)
	lines := checkWellFormed(t, raw)
	if v, _ := prop(lines, "METHOD"); v != "PUBLISH" {
		t.Fatalf("method %q", v)
	}
	if _, ok := prop(lines, "ATTENDEE"); ok {
		t.Fatal("PUBLISH must not list attendees")
	}
	if v, _ := prop(lines, "SUMMARY"); unescape(v) != m.Title {
		t.Fatalf("summary round trip %q", unescape(v))
	}
	if v, _ := prop(lines, "SUMMARY"); !strings.Contains(v, `12\; Ada\, Grace`) || !strings.Contains(v, `\\backslash`) {
		t.Fatalf("summary escaping %q", v)
	}
	if v, _ := prop(lines, "DESCRIPTION"); unescape(v) != Body(&m) || !strings.Contains(v, `\n`) {
		t.Fatalf("description round trip")
	}
	for name, want := range map[string]string{"DTSTART": "20261030T170000Z", "DTEND": "20261030T183000Z", "SEQUENCE": "0", "UID": m.UID, "STATUS": "CONFIRMED", "URL": guestLink, "DTSTAMP": "20261026T091500Z"} {
		if v, _ := prop(lines, name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	if v, _ := prop(lines, "LOCATION"); unescape(v) != guestLink {
		t.Errorf("location %q", v)
	}
}

func TestInviteRequestCancelAndSequence(t *testing.T) {
	m := fixture()
	m.Organizer = &Person{Name: `Ben "Host"`, Email: "ben@example.net"}
	m.Sequence = 2
	m.Updated = m.Created.Add(time.Hour)
	raw := InviteICS(&m)
	golden(t, "invite-request.ics", raw)
	lines := checkWellFormed(t, raw)
	if v, _ := prop(lines, "METHOD"); v != "REQUEST" {
		t.Fatalf("method %q", v)
	}
	if v, _ := prop(lines, "ORGANIZER"); v != "mailto:ben@example.net" {
		t.Fatalf("organizer %q", v)
	}
	var attendees int
	for _, l := range lines {
		if strings.HasPrefix(l, "ATTENDEE;") {
			attendees++
			if !strings.HasSuffix(l, ":mailto:ada@example.com") && !strings.HasSuffix(l, ":mailto:grace@example.org") {
				t.Errorf("attendee %q", l)
			}
		}
		if strings.HasPrefix(l, "ORGANIZER;") && !strings.Contains(l, `CN="Ben Host"`) {
			t.Errorf("organizer CN must be quoted without DQUOTE: %q", l)
		}
	}
	if attendees != 2 {
		t.Fatalf("attendees %d", attendees)
	}
	if v, _ := prop(lines, "SEQUENCE"); v != "2" {
		t.Fatalf("sequence %q", v)
	}

	m.Cancelled = true
	m.Sequence = 3
	raw = InviteICS(&m)
	golden(t, "invite-cancel.ics", raw)
	lines = checkWellFormed(t, raw)
	for name, want := range map[string]string{"METHOD": "CANCEL", "STATUS": "CANCELLED", "SEQUENCE": "3", "UID": m.UID} {
		if v, _ := prop(lines, name); v != want {
			t.Errorf("%s = %q, want %q", name, v, want)
		}
	}
	if _, ok := prop(lines, "BEGIN:VALARM"); ok || strings.Contains(string(raw), "VALARM") {
		t.Error("a cancellation carries no reminder")
	}

	// Without an organizer, a guest list alone stays PUBLISH.
	m = fixture()
	if InviteMethod(&m) != MethodPublish {
		t.Fatal("no organizer → PUBLISH")
	}
	m.GuestEmails = nil
	m.Organizer = &Person{Email: "ben@example.net"}
	if InviteMethod(&m) != MethodPublish {
		t.Fatal("no guests → PUBLISH")
	}
}

func TestFeedGolden(t *testing.T) {
	a, b := fixture(), fixture()
	b.UID, b.Title, b.Start, b.Duration, b.TimeZone, b.GuestEmails, b.Description = "mtg_second@studiolink", "Weekly sync", time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC), 30*time.Minute, "", nil, ""
	b.Organizer = &Person{Email: "ben@example.net"}
	raw := FeedICS("p2p.video — Ben", []Meeting{a, b})
	golden(t, "feed.ics", raw)
	lines := checkWellFormed(t, raw)
	if _, ok := prop(lines, "METHOD"); ok {
		t.Fatal("a subscription feed has no METHOD")
	}
	if _, ok := prop(lines, "ATTENDEE"); ok {
		t.Fatal("feed lists no attendees")
	}
	if _, ok := prop(lines, "ORGANIZER"); ok {
		t.Fatal("feed lists no organizer")
	}
	if v, _ := prop(lines, "X-WR-CALNAME"); v != "p2p.video — Ben" {
		t.Fatalf("calname %q", v)
	}
	if v, _ := prop(lines, "REFRESH-INTERVAL"); v != "PT15M" {
		t.Fatalf("refresh %q", v)
	}
	if n := strings.Count(string(raw), "BEGIN:VEVENT"); n != 2 {
		t.Fatalf("events %d", n)
	}
}

func TestFoldBoundaries(t *testing.T) {
	// Exactly 75 octets: no fold. 76: one fold.
	l75 := strings.Repeat("a", 75)
	if Fold(l75) != l75+"\r\n" {
		t.Fatal("75 octets must not fold")
	}
	if got := Fold(l75 + "b"); got != l75+"\r\n b\r\n" {
		t.Fatalf("76 octets: %q", got)
	}
	// A 4-octet rune straddling octet 75 moves whole to the next line.
	s := strings.Repeat("a", 73) + "🎙" + strings.Repeat("c", 80)
	out := Fold(s)
	for _, l := range strings.Split(strings.TrimSuffix(out, "\r\n"), "\r\n") {
		if len(l) > 75 || !utf8.ValidString(l) {
			t.Fatalf("bad physical line %q", l)
		}
	}
	if strings.ReplaceAll(strings.TrimSuffix(out, "\r\n"), "\r\n ", "") != s {
		t.Fatal("unfold must restore the line")
	}
	// Continuation lines hold at most 74 content octets (+1 space).
	long := strings.Repeat("x", 300)
	for i, l := range strings.Split(strings.TrimSuffix(Fold(long), "\r\n"), "\r\n") {
		if i > 0 && (len(l) != 75 && i < 4) {
			t.Fatalf("continuation %d is %d octets", i, len(l))
		}
	}
}

func TestEscapeTextDropsControls(t *testing.T) {
	if got := EscapeText("a\r\nb\tc\x07d\u202ee;f,g\\h"); got != `a\nb cde\;f\,g\\h` {
		t.Fatalf("%q", got)
	}
}

func TestInviteTextAndLinks(t *testing.T) {
	m := fixture()
	text := Text(&m)
	for _, want := range []string{m.Title, "Friday, October 30, 2026, 10:00 AM – 11:30 AM PDT (America/Los_Angeles)", "Join the call: " + guestLink, "Google Chrome", "headphones", "Agenda:"} {
		if !strings.Contains(text, want) {
			t.Errorf("invite text lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"127.0.0.1", "localhost", "launch=", "session=", "hostToken"} {
		if strings.Contains(text, bad) || strings.Contains(GoogleURL(&m), bad) || strings.Contains(OutlookURL(&m), bad) {
			t.Errorf("invite carries %q", bad)
		}
	}

	g, err := url.Parse(GoogleURL(&m))
	if err != nil || g.Host != "calendar.google.com" || g.Path != "/calendar/render" {
		t.Fatalf("google url %v", err)
	}
	q := g.Query()
	if q.Get("action") != "TEMPLATE" || q.Get("dates") != "20261030T170000Z/20261030T183000Z" || q.Get("text") != m.Title ||
		q.Get("location") != guestLink || q.Get("add") != "ada@example.com,grace@example.org" || q.Get("details") != Body(&m) || q.Get("ctz") != "America/Los_Angeles" {
		t.Fatalf("google query %v", q)
	}
	if strings.Contains(g.RawQuery, "+") {
		t.Fatal("spaces must be %20, not +")
	}

	o, _ := url.Parse(OutlookURL(&m))
	oq := o.Query()
	if o.Host != "outlook.live.com" || oq.Get("rru") != "addevent" || oq.Get("startdt") != "2026-10-30T17:00:00Z" || oq.Get("enddt") != "2026-10-30T18:30:00Z" || oq.Get("subject") != m.Title || oq.Get("location") != guestLink {
		t.Fatalf("outlook %v", oq)
	}

	mail := MailtoURL(&m)
	if !strings.HasPrefix(mail, "mailto:ada@example.com,grace@example.org?") {
		t.Fatalf("mailto %q", mail)
	}
	mq, _ := url.ParseQuery(strings.SplitN(mail, "?", 2)[1])
	if mq.Get("subject") != "Invitation: "+m.Title || !strings.Contains(mq.Get("body"), guestLink) {
		t.Fatalf("mailto query %v", mq)
	}
	m.GuestEmails = nil
	if MailtoURL(&m) != "" {
		t.Fatal("no guests, no mailto")
	}

	// No guest link: the invite says the host will send it.
	m.GuestURL = ""
	if !strings.Contains(Body(&m), "The host will send you the link") || strings.Contains(GoogleURL(&m), "location=") {
		t.Fatal("linkless invite")
	}
}

func TestWhenAcrossZonesAndDays(t *testing.T) {
	m := Meeting{Start: time.Date(2026, 3, 8, 9, 0, 0, 0, time.UTC), Duration: time.Hour, TimeZone: "America/New_York"}
	if got := When(&m); got != "Sunday, March 8, 2026, 5:00 AM – 6:00 AM EDT (America/New_York)" {
		t.Fatalf("DST day: %q", got)
	}
	m = Meeting{Start: time.Date(2026, 10, 30, 23, 30, 0, 0, time.UTC), Duration: time.Hour}
	if got := When(&m); got != "Friday, October 30, 2026, 11:30 PM – Saturday, October 31, 12:30 AM UTC" {
		t.Fatalf("UTC, crosses midnight: %q", got)
	}
	m.TimeZone = "Not/AZone"
	if Location(m.TimeZone) != time.UTC {
		t.Fatal("unknown zone falls back to UTC")
	}
}

func TestFilename(t *testing.T) {
	for in, want := range map[string]string{"Épisode 12; Ada & Grace!": "pisode-12-ada-grace.ics", "": "meeting.ics", "🎙️": "meeting.ics", "Weekly Sync": "weekly-sync.ics"} {
		if got := Filename(in); got != want {
			t.Errorf("Filename(%q) = %q, want %q", in, got, want)
		}
	}
}
