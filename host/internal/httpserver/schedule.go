package httpserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/omniharmonic/studiolink/host/internal/calendar"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Calendar integration [SL-P6-1, ADR-018]. No calendar service, no OAuth:
//
//   - /api/v1/schedule…: the host schedules meetings on rooms (management
//     only: admin credential, never Funnel). Each meeting yields a .ics
//     invite, Google/Outlook "add event" template links and invite text, all
//     built from the room's guest link, the only secret they carry.
//   - /api/v1/calendar/feed…: a rotatable feed secret (nonce + SHA-256
//     stored, the secret is derived like a room link) for an iCalendar
//     subscription of upcoming meetings. The feed itself
//     (/api/v1/calendar/feed.ics?key=…) is served to this Mac and to tailnet
//     devices through private Serve only; a Funnel request is refused even
//     with the right key.
//   - /api/v1/desktop/calendar: rooms, guest origins and meeting times for the
//     Mac app's EventKit matching; loopback + desktop token, no secrets.

const (
	maxMeetingTitleRunes = 120
	maxDescriptionRunes  = 4000
	maxGuestEmails       = 50
	minMeetingMinutes    = 5
	maxScheduleMinutes   = 12 * 60
	feedPastWindow       = 30 * 24 * time.Hour
	feedName             = "p2p.video meetings"
)

// emailPattern is deliberately conservative: addresses go into ICS
// ATTENDEE values, mailto: URLs and template links unquoted.
var emailPattern = regexp.MustCompile(`^[A-Za-z0-9._+\-]{1,64}@[A-Za-z0-9\-]{1,63}(\.[A-Za-z0-9\-]{1,63})+$`)

// MeetingInvite is everything the host needs to invite guests. GuestURL,
// and every link and text containing it, carry the room link secret:
// management responses only, never logged.
type MeetingInvite struct {
	When       string `json:"when"`
	Method     string `json:"method"`
	GuestURL   string `json:"guestUrl,omitempty"`
	Remote     bool   `json:"remote"`
	Text       string `json:"text"`
	GoogleURL  string `json:"googleUrl"`
	OutlookURL string `json:"outlookUrl"`
	MailtoURL  string `json:"mailtoUrl,omitempty"`
	ICSPath    string `json:"icsPath"`
}

// MeetingView is a scheduled meeting as the host dashboard sees it.
type MeetingView struct {
	store.ScheduledMeeting
	EndsAt       time.Time `json:"endsAt"`
	RoomName     string    `json:"roomName"`
	RoomArchived bool      `json:"roomArchived"`
	RoomLive     bool      `json:"roomLive"`
	// LinkReady is false while the room's guest link is off or lost: invites
	// for a scheduled meeting are refused until it is back.
	LinkReady bool          `json:"linkReady"`
	Invite    MeetingInvite `json:"invite"`
}

// roomGuestURL is the room's current guest link, or "" when it is off or
// the daemon key changed (LinkLost).
func (s *Server) roomGuestURL(room *store.Room, base guestBaseFn) (string, bool) {
	if room == nil || room.Archived || !room.LinkEnabled() {
		return "", false
	}
	secret := s.roomSecret(room.ID, room.InviteNonce)
	if hashToken(secret) != room.InviteSecretHash {
		return "", false
	}
	b, remote := base()
	return b + "/join?" + url.Values{"room": {room.Slug}, "token": {secret}}.Encode(), remote
}

func calendarMeeting(m *store.ScheduledMeeting, room *store.Room, guestURL string) calendar.Meeting {
	cm := calendar.Meeting{
		UID:         m.UID,
		Sequence:    m.Sequence,
		Title:       m.Title,
		Description: m.Description,
		Start:       m.StartsAt,
		Duration:    time.Duration(m.DurationMin) * time.Minute,
		TimeZone:    m.TimeZone,
		GuestURL:    guestURL,
		GuestEmails: m.GuestEmails,
		Cancelled:   m.Cancelled(),
		Created:     m.CreatedAt,
		Updated:     m.UpdatedAt,
	}
	if m.OrganizerEmail != "" {
		cm.Organizer = &calendar.Person{Email: m.OrganizerEmail}
		if room != nil {
			cm.Organizer.Name = room.Settings.HostName
		}
	}
	return cm
}

func (s *Server) meetingView(m *store.ScheduledMeeting, rooms map[string]*store.Room, base guestBaseFn) MeetingView {
	v := MeetingView{ScheduledMeeting: *m, EndsAt: m.EndsAt()}
	if v.GuestEmails == nil {
		v.GuestEmails = []string{}
	}
	room := rooms[m.RoomID]
	if room == nil {
		if r, err := s.Store.GetRoom(m.RoomID); err == nil {
			room = r
			rooms[m.RoomID] = r
		}
	}
	guestURL, remote := "", false
	if room != nil {
		v.RoomName, v.RoomArchived = room.Name, room.Archived
		if _, err := s.Store.LiveRoomSession(room.ID); err == nil {
			v.RoomLive = true
		}
		guestURL, remote = s.roomGuestURL(room, base)
	}
	v.LinkReady = guestURL != ""
	cm := calendarMeeting(m, room, guestURL)
	v.Invite = MeetingInvite{
		When:       calendar.When(&cm),
		Method:     string(calendar.InviteMethod(&cm)),
		GuestURL:   guestURL,
		Remote:     remote,
		Text:       calendar.Text(&cm),
		GoogleURL:  calendar.GoogleURL(&cm),
		OutlookURL: calendar.OutlookURL(&cm),
		MailtoURL:  calendar.MailtoURL(&cm),
		ICSPath:    "/api/v1/schedule/" + url.PathEscape(m.ID) + "/invite.ics",
	}
	return v
}

// meetingInput is the writable part of a scheduled meeting.
type meetingInput struct {
	RoomID         *string   `json:"roomId"`
	Title          *string   `json:"title"`
	Description    *string   `json:"description"`
	StartsAt       *string   `json:"startsAt"`
	DurationMin    *int      `json:"durationMin"`
	TimeZone       *string   `json:"timeZone"`
	GuestEmails    *[]string `json:"guestEmails"`
	OrganizerEmail *string   `json:"organizerEmail"`
}

type inputError string

func (e inputError) Error() string { return string(e) }

func cleanDescription(s string) (string, error) {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, ""), "\r\n", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxDescriptionRunes {
		return "", inputError(fmt.Sprintf("the description is limited to %d characters", maxDescriptionRunes))
	}
	return s, nil
}

func cleanEmail(addr string) (string, bool) {
	addr = strings.TrimSpace(addr)
	return addr, len(addr) <= 254 && emailPattern.MatchString(addr)
}

func cleanEmails(list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	seen := map[string]bool{}
	for _, raw := range list {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		addr, ok := cleanEmail(raw)
		if !ok {
			return nil, inputError(fmt.Sprintf("%q is not an email address p2p.video can put in an invite", truncateRunes(raw, 60)))
		}
		if key := strings.ToLower(addr); !seen[key] {
			seen[key] = true
			out = append(out, addr)
		}
	}
	if len(out) > maxGuestEmails {
		return nil, inputError(fmt.Sprintf("invite at most %d guests by email", maxGuestEmails))
	}
	return out, nil
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// apply validates in and writes it onto m. now bounds the start time.
func (in *meetingInput) apply(s *Server, m *store.ScheduledMeeting, now time.Time) error {
	if in.RoomID != nil && *in.RoomID != m.RoomID {
		room, err := s.Store.GetRoom(*in.RoomID)
		if err != nil {
			return inputError("choose a room for the meeting")
		}
		if room.Archived {
			return inputError("restore that room before scheduling in it")
		}
		m.RoomID = room.ID
	}
	if in.Title != nil {
		title, ok := cleanName(*in.Title, maxMeetingTitleRunes)
		if !ok {
			return inputError(fmt.Sprintf("the title must be 1-%d characters without control characters", maxMeetingTitleRunes))
		}
		m.Title = title
	}
	if in.Description != nil {
		d, err := cleanDescription(*in.Description)
		if err != nil {
			return err
		}
		m.Description = d
	}
	if in.StartsAt != nil {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(*in.StartsAt))
		if err != nil {
			return inputError("startsAt must be an RFC 3339 time, e.g. 2026-10-30T17:00:00Z")
		}
		t = t.UTC().Truncate(time.Second)
		if !t.Equal(m.StartsAt) && (t.Before(now.Add(-24*time.Hour)) || t.After(now.Add(2*366*24*time.Hour))) {
			return inputError("schedule the meeting between yesterday and two years from now")
		}
		m.StartsAt = t
	}
	if in.DurationMin != nil {
		if *in.DurationMin < minMeetingMinutes || *in.DurationMin > maxScheduleMinutes {
			return inputError(fmt.Sprintf("the duration must be %d-%d minutes", minMeetingMinutes, maxScheduleMinutes))
		}
		m.DurationMin = *in.DurationMin
	}
	if in.TimeZone != nil {
		tz := strings.TrimSpace(*in.TimeZone)
		if tz == "" {
			tz = "UTC"
		}
		if len(tz) > 64 {
			return inputError("unknown time zone")
		}
		if _, err := time.LoadLocation(tz); err != nil || tz == "Local" {
			return inputError("unknown time zone")
		}
		m.TimeZone = tz
	}
	if in.GuestEmails != nil {
		list, err := cleanEmails(*in.GuestEmails)
		if err != nil {
			return err
		}
		m.GuestEmails = list
	}
	if in.OrganizerEmail != nil {
		addr := strings.TrimSpace(*in.OrganizerEmail)
		if addr != "" {
			var ok bool
			if addr, ok = cleanEmail(addr); !ok {
				return inputError("your email address doesn't look right")
			}
		}
		m.OrganizerEmail = addr
	}
	return nil
}

// sameForGuests reports whether nothing guests would see changed.
func sameForGuests(a, b *store.ScheduledMeeting) bool {
	return a.RoomID == b.RoomID && a.Title == b.Title && a.Description == b.Description && a.StartsAt.Equal(b.StartsAt) &&
		a.DurationMin == b.DurationMin && a.TimeZone == b.TimeZone && a.OrganizerEmail == b.OrganizerEmail &&
		strings.Join(a.GuestEmails, ",") == strings.Join(b.GuestEmails, ",")
}

// handleSchedule serves /api/v1/schedule and /api/v1/schedule/... (host
// management only).
func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.adminAllowed(r) {
		adminDenied(w, r, "Scheduling meetings")
		return
	}
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/schedule"), "/")
	if rel == "" {
		switch r.Method {
		case http.MethodGet:
			s.listSchedule(w, r)
		case http.MethodPost:
			s.createScheduled(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	id, action, _ := strings.Cut(rel, "/")
	m, err := s.Store.GetScheduledMeeting(id)
	if err != nil {
		http.Error(w, "meeting not found", http.StatusNotFound)
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeSecretJSON(w, http.StatusOK, s.meetingView(m, map[string]*store.Room{}, s.memoBase(r)))
	case action == "" && r.Method == http.MethodPatch:
		s.updateScheduled(w, r, m)
	case action == "" && r.Method == http.MethodDelete:
		if err := s.Store.DeleteScheduledMeeting(m.ID); err != nil {
			http.Error(w, "cannot delete the meeting", http.StatusInternalServerError)
			return
		}
		slog.Info("Scheduled meeting deleted", "meetingId", m.ID)
		w.WriteHeader(http.StatusNoContent)
	case action == "cancel" && r.Method == http.MethodPost:
		if !m.Cancelled() {
			m.Status = store.MeetingCancelled
			m.Sequence++
			if err := s.Store.UpdateScheduledMeeting(m); err != nil {
				http.Error(w, "cannot cancel the meeting", http.StatusInternalServerError)
				return
			}
			slog.Info("Scheduled meeting cancelled", "meetingId", m.ID, "sequence", m.Sequence)
		}
		writeSecretJSON(w, http.StatusOK, s.meetingView(m, map[string]*store.Room{}, s.memoBase(r)))
	case action == "invite.ics" && r.Method == http.MethodGet:
		s.serveInviteICS(w, r, m)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func (s *Server) listSchedule(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListScheduledMeetings(time.Now(), true)
	if err != nil {
		slog.Error("list scheduled meetings failed", "err", err)
		http.Error(w, "cannot list meetings", http.StatusInternalServerError)
		return
	}
	base := s.memoBase(r)
	rooms := map[string]*store.Room{}
	out := make([]MeetingView, 0, len(list))
	for i := range list {
		out = append(out, s.meetingView(&list[i], rooms, base))
	}
	writeSecretJSON(w, http.StatusOK, map[string]any{"meetings": out})
}

func (s *Server) createScheduled(w http.ResponseWriter, r *http.Request) {
	var in meetingInput
	if !decodeBody(w, r, &in) {
		return
	}
	if in.RoomID == nil || in.StartsAt == nil {
		http.Error(w, "roomId and startsAt are required", http.StatusBadRequest)
		return
	}
	room, err := s.Store.GetRoom(*in.RoomID)
	if err != nil {
		http.Error(w, "choose a room for the meeting", http.StatusBadRequest)
		return
	}
	if room.Archived {
		http.Error(w, "restore that room before scheduling in it", http.StatusConflict)
		return
	}
	if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
		in.Title = &room.Name
	}
	if in.DurationMin == nil {
		d := 60
		in.DurationMin = &d
	}
	if in.TimeZone == nil {
		tz := "UTC"
		in.TimeZone = &tz
	}
	now := time.Now().UTC()
	id, err1 := randomHex(6)
	uid, err2 := randomHex(16)
	if err1 != nil || err2 != nil {
		http.Error(w, "cannot schedule the meeting", http.StatusInternalServerError)
		return
	}
	m := &store.ScheduledMeeting{ID: "mtg_" + id, UID: uid + "@studiolink", Status: store.MeetingScheduled, CreatedAt: now, UpdatedAt: now, GuestEmails: []string{}}
	if err := in.apply(s, m, now); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.Store.CreateScheduledMeeting(m); err != nil {
		slog.Error("schedule meeting failed", "err", err)
		http.Error(w, "cannot schedule the meeting", http.StatusInternalServerError)
		return
	}
	slog.Info("Meeting scheduled", "meetingId", m.ID, "roomId", m.RoomID)
	writeSecretJSON(w, http.StatusCreated, s.meetingView(m, map[string]*store.Room{}, s.memoBase(r)))
}

func (s *Server) updateScheduled(w http.ResponseWriter, r *http.Request, m *store.ScheduledMeeting) {
	var in meetingInput
	if !decodeBody(w, r, &in) {
		return
	}
	if m.Cancelled() {
		http.Error(w, "this meeting was cancelled; schedule a new one", http.StatusConflict)
		return
	}
	before := *m
	before.GuestEmails = append([]string(nil), m.GuestEmails...)
	if err := in.apply(s, m, time.Now().UTC()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !sameForGuests(&before, m) {
		// Calendars replace an event only when SEQUENCE goes up.
		m.Sequence++
		if err := s.Store.UpdateScheduledMeeting(m); err != nil {
			http.Error(w, "cannot update the meeting", http.StatusInternalServerError)
			return
		}
		slog.Info("Scheduled meeting updated", "meetingId", m.ID, "sequence", m.Sequence)
	}
	writeSecretJSON(w, http.StatusOK, s.meetingView(m, map[string]*store.Room{}, s.memoBase(r)))
}

// roomLinkChanged bumps the SEQUENCE of the room's upcoming scheduled
// meetings after its guest link was replaced or turned off, so a re-sent
// invite or the feed replaces the event that carried the old link
// [SL-P5-4]. Cancelled and past meetings are left alone.
func (s *Server) roomLinkChanged(roomID string) {
	meetings, err := s.Store.ListScheduledMeetings(time.Now(), false)
	if err != nil {
		slog.Warn("Cannot update invites after a room link change", "roomId", roomID, "err", err)
		return
	}
	for i := range meetings {
		m := &meetings[i]
		if m.RoomID != roomID {
			continue
		}
		m.Sequence++
		if err := s.Store.UpdateScheduledMeeting(m); err != nil {
			slog.Warn("Cannot bump invite sequence", "meetingId", m.ID, "err", err)
			continue
		}
		slog.Info("Scheduled meeting link changed", "meetingId", m.ID, "sequence", m.Sequence)
	}
}

func (s *Server) serveInviteICS(w http.ResponseWriter, r *http.Request, m *store.ScheduledMeeting) {
	room, _ := s.Store.GetRoom(m.RoomID)
	guestURL, _ := s.roomGuestURL(room, s.memoBase(r))
	if guestURL == "" && !m.Cancelled() {
		http.Error(w, "the room's guest link is off; turn it on (New link) before sending invites", http.StatusConflict)
		return
	}
	cm := calendarMeeting(m, room, guestURL)
	body := calendar.InviteICS(&cm)
	method := strings.ToLower(string(calendar.InviteMethod(&cm)))
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8; method="+strings.ToUpper(method))
	w.Header().Set("Content-Disposition", `attachment; filename="`+calendar.Filename(m.Title)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------- the feed

// feedSecret derives the calendar feed secret from its current nonce.
func (s *Server) feedSecret(nonce string) string {
	m := hmac.New(sha256.New, s.Config.SecretKey)
	m.Write([]byte("studiolink/calendar-feed/v1\x00" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// CalendarFeedView is the feed's management view. URLs carry the feed
// secret: management responses only, never logged.
type CalendarFeedView struct {
	Enabled bool `json:"enabled"`
	// Lost: the daemon key changed; rotate for a new address.
	Lost bool `json:"lost,omitempty"`
	// URL is the address for this Mac and, when Remote, tailnet devices.
	URL       string `json:"url,omitempty"`
	WebcalURL string `json:"webcalUrl,omitempty"`
	LocalURL  string `json:"localUrl,omitempty"`
	Remote    bool   `json:"remote"`
}

const feedPath = "/api/v1/calendar/feed.ics"

func (s *Server) feedView(r *http.Request) (CalendarFeedView, error) {
	nonce, hash, err := s.Store.CalendarFeed()
	if err != nil {
		return CalendarFeedView{}, err
	}
	v := CalendarFeedView{Enabled: nonce != "" && hash != ""}
	if !v.Enabled {
		return v, nil
	}
	secret := s.feedSecret(nonce)
	if hashToken(secret) != hash {
		v.Lost = true
		return v, nil
	}
	path := feedPath + "?" + url.Values{"key": {secret}}.Encode()
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	v.LocalURL = scheme + "://" + r.Host + path
	base, remote := s.guestBase(r)
	v.URL, v.Remote = base+path, remote
	if u, err := url.Parse(v.URL); err == nil {
		u.Scheme = "webcal"
		v.WebcalURL = u.String()
	}
	return v, nil
}

// handleCalendar serves /api/v1/calendar/feed (view), …/rotate, …/revoke
// (management) and the feed itself, /api/v1/calendar/feed.ics.
func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	rel := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/calendar"), "/")
	if rel == "feed.ics" {
		s.serveFeed(w, r)
		return
	}
	if !s.adminAllowed(r) {
		adminDenied(w, r, "The calendar feed")
		return
	}
	switch {
	case rel == "feed" && r.Method == http.MethodGet:
	case rel == "feed/rotate" && r.Method == http.MethodPost:
		nonce, err := randomHex(16)
		if err == nil {
			err = s.Store.SetCalendarFeed(nonce, hashToken(s.feedSecret(nonce)))
		}
		if err != nil {
			http.Error(w, "cannot create the calendar feed address", http.StatusInternalServerError)
			return
		}
		slog.Info("Calendar feed address replaced")
	case rel == "feed/revoke" && r.Method == http.MethodPost:
		if err := s.Store.SetCalendarFeed("", ""); err != nil {
			http.Error(w, "cannot turn off the calendar feed", http.StatusInternalServerError)
			return
		}
		slog.Info("Calendar feed turned off")
	case rel == "feed" || rel == "feed/rotate" || rel == "feed/revoke":
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	v, err := s.feedView(r)
	if err != nil {
		http.Error(w, "cannot read the calendar feed", http.StatusInternalServerError)
		return
	}
	writeSecretJSON(w, http.StatusOK, v)
}

// tailnetAddr reports whether ip is a Tailscale address (CGNAT range or the
// Tailscale ULA prefix).
func tailnetAddr(ip net.IP) bool {
	_, v4, _ := net.ParseCIDR("100.64.0.0/10")
	_, v6, _ := net.ParseCIDR("fd7a:115c:a1e0::/48")
	return ip != nil && (v4.Contains(ip) || v6.Contains(ip))
}

// feedPositionAllowed admits a direct loopback request (this Mac) or a
// private Tailscale Serve request from a tailnet device. Never Funnel:
// Funnel traffic also reaches the daemon over loopback, so its marker
// header is refused explicitly, whatever the key.
func feedPositionAllowed(r *http.Request) bool {
	if r.Header.Get("Tailscale-Funnel-Request") != "" {
		return false
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(remote); err != nil || ip == nil || !ip.IsLoopback() {
		return false
	}
	if localManagement(r) {
		return true
	}
	return tailnetAddr(net.ParseIP(strings.TrimSpace(r.Header.Get("X-Forwarded-For"))))
}

// serveFeed (GET /api/v1/calendar/feed.ics?key=<feed secret>) is the host's
// iCalendar subscription: upcoming (and last 30 days') meetings that are not
// cancelled. Calendar apps cannot send headers, so the key is in the URL.
func (s *Server) serveFeed(w http.ResponseWriter, r *http.Request) {
	if !feedPositionAllowed(r) {
		http.Error(w, "the calendar feed is available only on this Mac and your tailnet", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nonce, hash, err := s.Store.CalendarFeed()
	key := r.URL.Query().Get("key")
	if err != nil || nonce == "" || hash == "" || !tokenHashMatches(key, hash) || subtle.ConstantTimeCompare([]byte(key), []byte(s.feedSecret(nonce))) != 1 {
		http.Error(w, "this calendar feed address is not valid any more", http.StatusUnauthorized)
		return
	}
	list, err := s.Store.ListScheduledMeetings(time.Now().Add(-feedPastWindow), false)
	if err != nil {
		http.Error(w, "calendar unavailable", http.StatusInternalServerError)
		return
	}
	base := s.memoBase(r)
	rooms := map[string]*store.Room{}
	meetings := make([]calendar.Meeting, 0, len(list))
	for i := range list {
		m := &list[i]
		room, ok := rooms[m.RoomID]
		if !ok {
			room, _ = s.Store.GetRoom(m.RoomID)
			rooms[m.RoomID] = room
		}
		guestURL, _ := s.roomGuestURL(room, base)
		meetings = append(meetings, calendarMeeting(m, room, guestURL))
	}
	body := calendar.FeedICS(feedName, meetings)
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="p2p.video.ics"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// ------------------------------------------------------- desktop matching

// DesktopCalendar feeds the Mac app's calendar matching (EventKit): which
// room links are this host's (slug + origin) and what is scheduled. No
// link secrets, host tokens or guest addresses.
type DesktopCalendar struct {
	Rooms        []DesktopCalendarRoom     `json:"rooms"`
	GuestOrigins []string                  `json:"guestOrigins"`
	Scheduled    []DesktopScheduledMeeting `json:"scheduled"`
}

// DesktopCalendarRoom identifies a room by its public slug.
type DesktopCalendarRoom struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Live bool   `json:"live"`
}

// DesktopScheduledMeeting is a scheduled meeting's timing and identity.
type DesktopScheduledMeeting struct {
	ID       string    `json:"id"`
	RoomID   string    `json:"roomId"`
	Title    string    `json:"title"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
	Status   string    `json:"status"`
	Sequence int       `json:"sequence"`
	UID      string    `json:"uid"`
}

// guestOrigins lists every origin this host's guest links can carry.
func (s *Server) guestOrigins(r *http.Request) []string {
	seen := map[string]bool{}
	var out []string
	add := func(o string) {
		o = strings.TrimRight(o, "/")
		if u, err := url.Parse(o); err == nil && u.Host != "" && !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if b, _ := s.guestBase(r); b != "" {
		add(b)
	}
	if s.Config != nil && s.Config.PublicURL != "" {
		add(s.Config.PublicURL)
	}
	if s.Config != nil && s.Config.HTTPPort > 0 {
		add(fmt.Sprintf("http://127.0.0.1:%d", s.Config.HTTPPort))
		add(fmt.Sprintf("http://localhost:%d", s.Config.HTTPPort))
	}
	add("http://" + r.Host)
	return out
}

// handleDesktopCalendar (GET /api/v1/desktop/calendar): direct loopback with
// the desktop token, like /api/v1/desktop/state.
func (s *Server) handleDesktopCalendar(w http.ResponseWriter, r *http.Request) {
	if !desktopRequestAllowed(r) {
		http.Error(w, "desktop calendar is available only to the local host app", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.DesktopToken == "" {
		http.Error(w, "desktop control is unavailable", http.StatusServiceUnavailable)
		return
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.DesktopToken)) != 1 {
		http.Error(w, "desktop authorization required", http.StatusUnauthorized)
		return
	}
	out := DesktopCalendar{Rooms: []DesktopCalendarRoom{}, Scheduled: []DesktopScheduledMeeting{}, GuestOrigins: s.guestOrigins(r)}
	rooms, err := s.Store.ListRooms(false)
	if err != nil {
		http.Error(w, "desktop calendar unavailable", http.StatusInternalServerError)
		return
	}
	for _, room := range rooms {
		_, liveErr := s.Store.LiveRoomSession(room.ID)
		out.Rooms = append(out.Rooms, DesktopCalendarRoom{ID: room.ID, Name: room.Name, Slug: room.Slug, Live: liveErr == nil})
	}
	list, err := s.Store.ListScheduledMeetings(time.Now(), true)
	if err != nil {
		http.Error(w, "desktop calendar unavailable", http.StatusInternalServerError)
		return
	}
	for _, m := range list {
		out.Scheduled = append(out.Scheduled, DesktopScheduledMeeting{ID: m.ID, RoomID: m.RoomID, Title: m.Title, StartsAt: m.StartsAt, EndsAt: m.EndsAt(), Status: m.Status, Sequence: m.Sequence, UID: m.UID})
	}
	w.Header().Set("Content-Type", "application/json")
	writeSecretJSON(w, http.StatusOK, out)
}
