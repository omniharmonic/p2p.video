// Package calendar renders scheduled meetings as RFC 5545 iCalendar data,
// plain-text invites and "add to calendar" template links [SL-P6-1].
//
// Nothing here talks to a calendar service: invites are files and plain URLs
// the host hands to their own calendar or mail client. The only secret an
// invite may carry is the room's guest link; host and admin URLs never appear.
package calendar

import (
	"bytes"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ProdID identifies p2p.video as the producer of every calendar object.
const ProdID = "-//p2p.video//p2p.video Calendar 1.0//EN"

// Method is the iTIP method (RFC 5546) of a calendar object.
type Method string

const (
	// MethodNone omits METHOD, as a subscription feed should.
	MethodNone Method = ""
	// MethodPublish: an event to add, no RSVP expected (no ORGANIZER needed).
	MethodPublish Method = "PUBLISH"
	// MethodRequest: an invitation with an ORGANIZER and ATTENDEEs.
	MethodRequest Method = "REQUEST"
	// MethodCancel: the event (UID, higher SEQUENCE) is cancelled.
	MethodCancel Method = "CANCEL"
)

// Person is an organizer or attendee address.
type Person struct {
	Name  string
	Email string
}

// Event is one VEVENT. Times are written in UTC.
type Event struct {
	UID      string
	Sequence int
	// Stamp is DTSTAMP; Created and LastModified are optional.
	Stamp        time.Time
	Created      time.Time
	LastModified time.Time
	Start, End   time.Time
	Summary      string
	Description  string
	Location     string
	URL          string
	Cancelled    bool
	Organizer    *Person
	Attendees    []Person
	// AlarmMinutes adds a display reminder this many minutes before the
	// start; 0 adds none.
	AlarmMinutes int
}

// Calendar is one VCALENDAR object.
type Calendar struct {
	Method Method
	// Name is the calendar's display name (X-WR-CALNAME, RFC 7986 NAME);
	// for subscription feeds.
	Name string
	// Refresh suggests how often a subscriber polls (REFRESH-INTERVAL,
	// X-PUBLISHED-TTL); 0 omits it.
	Refresh time.Duration
	Events  []Event
}

// maxLineOctets is the RFC 5545 §3.1 line length limit, CRLF excluded.
const maxLineOctets = 75

// Encode renders the calendar with CRLF line endings, TEXT escaping and
// lines folded at 75 octets without splitting a UTF-8 sequence.
func (c *Calendar) Encode() []byte {
	var b bytes.Buffer
	w := writer{&b}
	w.line("BEGIN:VCALENDAR")
	w.line("VERSION:2.0")
	w.line("PRODID:" + ProdID)
	w.line("CALSCALE:GREGORIAN")
	if c.Method != MethodNone {
		w.line("METHOD:" + string(c.Method))
	}
	if c.Name != "" {
		w.line("NAME:" + EscapeText(c.Name))
		w.line("X-WR-CALNAME:" + EscapeText(c.Name))
	}
	if c.Refresh > 0 {
		d := durationValue(c.Refresh)
		w.line("REFRESH-INTERVAL;VALUE=DURATION:" + d)
		w.line("X-PUBLISHED-TTL:" + d)
	}
	for i := range c.Events {
		c.Events[i].encode(w)
	}
	w.line("END:VCALENDAR")
	return b.Bytes()
}

func (e *Event) encode(w writer) {
	w.line("BEGIN:VEVENT")
	w.line("UID:" + EscapeText(e.UID))
	w.line("DTSTAMP:" + UTCStamp(e.Stamp))
	if !e.Created.IsZero() {
		w.line("CREATED:" + UTCStamp(e.Created))
	}
	if !e.LastModified.IsZero() {
		w.line("LAST-MODIFIED:" + UTCStamp(e.LastModified))
	}
	w.line(fmt.Sprintf("SEQUENCE:%d", e.Sequence))
	w.line("DTSTART:" + UTCStamp(e.Start))
	w.line("DTEND:" + UTCStamp(e.End))
	w.line("SUMMARY:" + EscapeText(e.Summary))
	if e.Description != "" {
		w.line("DESCRIPTION:" + EscapeText(e.Description))
	}
	if e.Location != "" {
		w.line("LOCATION:" + EscapeText(e.Location))
	}
	if e.URL != "" {
		w.line("URL:" + uriValue(e.URL))
	}
	if e.Organizer != nil && e.Organizer.Email != "" {
		w.line("ORGANIZER" + cnParam(e.Organizer.Name) + ":mailto:" + uriValue(e.Organizer.Email))
	}
	for _, a := range e.Attendees {
		if a.Email == "" {
			continue
		}
		w.line("ATTENDEE" + cnParam(a.Name) + ";ROLE=REQ-PARTICIPANT;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:" + uriValue(a.Email))
	}
	if e.Cancelled {
		w.line("STATUS:CANCELLED")
	} else {
		w.line("STATUS:CONFIRMED")
	}
	w.line("TRANSP:OPAQUE")
	if e.AlarmMinutes > 0 && !e.Cancelled {
		w.line("BEGIN:VALARM")
		w.line("ACTION:DISPLAY")
		w.line("DESCRIPTION:" + EscapeText(e.Summary))
		w.line(fmt.Sprintf("TRIGGER:-PT%dM", e.AlarmMinutes))
		w.line("END:VALARM")
	}
	w.line("END:VEVENT")
}

type writer struct{ b *bytes.Buffer }

// line writes one content line, folded, with CRLF.
func (w writer) line(s string) { w.b.WriteString(Fold(s)) }

// Fold splits a content line into CRLF-terminated physical lines of at most
// 75 octets; continuation lines start with one space (which counts toward
// the 75). A UTF-8 sequence is never split.
func Fold(s string) string {
	var b strings.Builder
	limit := maxLineOctets
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		b.WriteString(s[:cut])
		b.WriteString("\r\n ")
		s = s[cut:]
		limit = maxLineOctets - 1
	}
	b.WriteString(s)
	b.WriteString("\r\n")
	return b.String()
}

// EscapeText escapes an RFC 5545 TEXT value: backslash, semicolon, comma and
// newlines; other control characters are dropped.
func EscapeText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == ';':
			b.WriteString(`\;`)
		case r == ',':
			b.WriteString(`\,`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			// Bidi overrides and other controls never reach a calendar.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// uriValue drops characters a URI or mail address property must not carry.
func uriValue(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '"' {
			return -1
		}
		return r
	}, s)
}

// cnParam renders ;CN="name" (quoted; DQUOTE and controls removed), or "".
func cnParam(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == '"' || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	return `;CN="` + name + `"`
}

// UTCStamp formats a DATE-TIME in UTC form (e.g. 20261030T170000Z).
func UTCStamp(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// durationValue renders a positive duration as an RFC 5545 DURATION (PT…M).
func durationValue(d time.Duration) string {
	if m := int(d / time.Minute); m >= 1 {
		return fmt.Sprintf("PT%dM", m)
	}
	return "PT1M"
}
