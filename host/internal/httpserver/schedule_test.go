package httpserver

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
)

func (h *roomsHarness) schedule(body string) (int, MeetingView, string) {
	h.t.Helper()
	code, raw := h.do("POST", "/api/v1/schedule", body, nil)
	var v MeetingView
	_ = json.Unmarshal(raw, &v)
	return code, v, string(raw)
}

func (h *roomsHarness) meetingCall(method, path, body string) (int, MeetingView, string) {
	h.t.Helper()
	code, raw := h.do(method, path, body, nil)
	var v MeetingView
	_ = json.Unmarshal(raw, &v)
	return code, v, string(raw)
}

// serve sends a request through the handler as Tailscale would deliver it:
// over loopback, with the given Host and headers (no default credential).
func (h *roomsHarness) serve(method, target string, hdr http.Header) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "127.0.0.1:40000"
	for k, v := range hdr {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, r)
	return w
}

func startIn(d time.Duration) string {
	return time.Now().UTC().Add(d).Truncate(time.Minute).Format(time.RFC3339)
}

// unfoldICS undoes RFC 5545 folding for assertions.
func unfoldICS(s string) []string {
	return strings.Split(strings.TrimSuffix(strings.ReplaceAll(s, "\r\n ", ""), "\r\n"), "\r\n")
}

func icsProp(lines []string, name string) string {
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, ":"); ok && (k == name || strings.HasPrefix(k, name+";")) {
			return v
		}
	}
	return ""
}

func TestScheduleInviteLifecycle(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	secret := secretOf(t, room)
	start := startIn(48 * time.Hour)
	code, m, raw := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"durationMin":90,"timeZone":"America/Los_Angeles","description":"Agenda: intros, compilers","guestEmails":["ada@example.com"," Grace@Example.org ","ada@example.com",""]}`, room.ID, start))
	if code != http.StatusCreated {
		t.Fatalf("schedule: %d %s", code, raw)
	}
	if m.Title != "The Podcast" || m.RoomName != "The Podcast" || m.DurationMin != 90 || m.Sequence != 0 || m.Status != "scheduled" || !m.LinkReady {
		t.Fatalf("view: %+v", m)
	}
	if len(m.GuestEmails) != 2 || m.GuestEmails[1] != "Grace@Example.org" {
		t.Fatalf("emails trimmed and deduplicated: %v", m.GuestEmails)
	}
	if !strings.HasPrefix(m.ID, "mtg_") || !strings.HasSuffix(m.UID, "@studiolink") || m.EndsAt.Sub(m.StartsAt) != 90*time.Minute {
		t.Fatalf("ids/times: %+v", m)
	}
	inv := m.Invite
	if inv.GuestURL != room.GuestURL || !inv.Remote || inv.Method != "PUBLISH" || !strings.Contains(inv.Text, room.GuestURL) || !strings.Contains(inv.Text, "Chrome") || !strings.Contains(inv.Text, "headphones") {
		t.Fatalf("invite: %+v", inv)
	}
	if !strings.Contains(inv.When, "(America/Los_Angeles)") {
		t.Fatalf("when: %q", inv.When)
	}
	g, _ := url.Parse(inv.GoogleURL)
	if g.Host != "calendar.google.com" || g.Query().Get("location") != room.GuestURL || g.Query().Get("add") != "ada@example.com,Grace@Example.org" {
		t.Fatalf("google: %s", inv.GoogleURL)
	}
	if !strings.HasPrefix(inv.OutlookURL, "https://outlook.live.com/") || !strings.HasPrefix(inv.MailtoURL, "mailto:ada@example.com,Grace@Example.org?") {
		t.Fatalf("outlook/mailto: %s %s", inv.OutlookURL, inv.MailtoURL)
	}
	// Nothing host-side in any invite: no host token, no launch, no dashboard.
	for _, s := range []string{inv.Text, inv.GoogleURL, inv.OutlookURL, inv.MailtoURL} {
		for _, bad := range []string{"127.0.0.1", "localhost", "launch=", "session=", testDesktopToken} {
			if strings.Contains(s, bad) {
				t.Fatalf("invite leaks %q: %s", bad, s)
			}
		}
	}

	// The .ics download.
	resp := h.rawDo("GET", inv.ICSPath, http.Header{"Authorization": {"Bearer " + testDesktopToken}})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/calendar") || !strings.Contains(resp.Header.Get("Content-Disposition"), `filename="the-podcast.ics"`) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("ics: %d %v", resp.StatusCode, resp.Header)
	}
	_, body := h.do("GET", inv.ICSPath, "", nil)
	lines := unfoldICS(string(body))
	if icsProp(lines, "METHOD") != "PUBLISH" || icsProp(lines, "SEQUENCE") != "0" || icsProp(lines, "UID") != m.UID || icsProp(lines, "URL") != room.GuestURL ||
		icsProp(lines, "DTSTART") != m.StartsAt.UTC().Format("20060102T150405Z") || strings.Contains(string(body), "ATTENDEE") {
		t.Fatalf("ics content:\n%s", body)
	}

	// An edit bumps SEQUENCE; the same values again do not.
	code, m, raw = h.meetingCall("PATCH", "/api/v1/schedule/"+m.ID, `{"title":"Episode 12","organizerEmail":"ben@example.net"}`)
	if code != 200 || m.Sequence != 1 || m.Title != "Episode 12" || m.Invite.Method != "REQUEST" {
		t.Fatalf("patch: %d %s", code, raw)
	}
	code, m, _ = h.meetingCall("PATCH", "/api/v1/schedule/"+m.ID, `{"title":"Episode 12"}`)
	if code != 200 || m.Sequence != 1 {
		t.Fatalf("no-op patch bumped sequence: %d", m.Sequence)
	}
	_, body = h.do("GET", inv.ICSPath, "", nil)
	lines = unfoldICS(string(body))
	if icsProp(lines, "METHOD") != "REQUEST" || icsProp(lines, "SEQUENCE") != "1" || icsProp(lines, "ORGANIZER") != "mailto:ben@example.net" || !strings.Contains(strings.Join(lines, "\n"), "RSVP=TRUE:mailto:ada@example.com") {
		t.Fatalf("request ics:\n%s", body)
	}

	// Cancel: SEQUENCE up, METHOD:CANCEL, STATUS:CANCELLED; idempotent.
	code, m, _ = h.meetingCall("POST", "/api/v1/schedule/"+m.ID+"/cancel", "")
	if code != 200 || m.Status != "cancelled" || m.Sequence != 2 || m.Invite.Method != "CANCEL" {
		t.Fatalf("cancel: %d %+v", code, m)
	}
	if code, m, _ = h.meetingCall("POST", "/api/v1/schedule/"+m.ID+"/cancel", ""); code != 200 || m.Sequence != 2 {
		t.Fatalf("second cancel: %d %d", code, m.Sequence)
	}
	_, body = h.do("GET", inv.ICSPath, "", nil)
	lines = unfoldICS(string(body))
	if icsProp(lines, "METHOD") != "CANCEL" || icsProp(lines, "STATUS") != "CANCELLED" || icsProp(lines, "SEQUENCE") != "2" || icsProp(lines, "UID") != m.UID {
		t.Fatalf("cancel ics:\n%s", body)
	}
	if code, _, _ := h.meetingCall("PATCH", "/api/v1/schedule/"+m.ID, `{"title":"x"}`); code != http.StatusConflict {
		t.Fatalf("editing a cancelled meeting: %d", code)
	}
	// Listed (cancelled too) until deleted.
	_, raw2 := h.do("GET", "/api/v1/schedule", "", nil)
	var list struct{ Meetings []MeetingView }
	_ = json.Unmarshal(raw2, &list)
	if len(list.Meetings) != 1 || list.Meetings[0].Status != "cancelled" {
		t.Fatalf("list: %s", raw2)
	}
	if code, _ := h.do("DELETE", "/api/v1/schedule/"+m.ID, "", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := h.do("GET", "/api/v1/schedule/"+m.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("deleted meeting: %d", code)
	}

	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "token=") || strings.Contains(logs.String(), "calendar.google.com") {
		t.Fatalf("invite links logged: %s", logs.String())
	}
}

func TestScheduleValidationAndRoomState(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("Weekly sync")
	ok := startIn(time.Hour)
	for name, body := range map[string]string{
		"no room":       fmt.Sprintf(`{"startsAt":%q}`, ok),
		"unknown room":  fmt.Sprintf(`{"roomId":"room_nope","startsAt":%q}`, ok),
		"no start":      fmt.Sprintf(`{"roomId":%q}`, room.ID),
		"bad start":     fmt.Sprintf(`{"roomId":%q,"startsAt":"tomorrow at 5"}`, room.ID),
		"long ago":      fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, startIn(-72*time.Hour)),
		"far future":    fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, startIn(3*366*24*time.Hour)),
		"too short":     fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"durationMin":1}`, room.ID, ok),
		"too long":      fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"durationMin":1000}`, room.ID, ok),
		"bad zone":      fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"timeZone":"Mars/Olympus"}`, room.ID, ok),
		"local zone":    fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"timeZone":"Local"}`, room.ID, ok),
		"bad email":     fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"guestEmails":["ada at example"]}`, room.ID, ok),
		"mailto inject": fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"guestEmails":["a@example.com?bcc=x@evil.com"]}`, room.ID, ok),
		"bad organizer": fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"organizerEmail":"me"}`, room.ID, ok),
		"control title": fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"title":"a‮b"}`, room.ID, ok),
		"long text":     fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"description":%q}`, room.ID, ok, strings.Repeat("x", 4001)),
		"unknown field": fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"hostToken":"x"}`, room.ID, ok),
	} {
		if code, _, raw := h.schedule(body); code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	emails := make([]string, 51)
	for i := range emails {
		emails[i] = fmt.Sprintf("g%d@example.com", i)
	}
	eb, _ := json.Marshal(emails)
	if code, _, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"guestEmails":%s}`, room.ID, ok, eb)); code != http.StatusBadRequest {
		t.Fatalf("51 guests: %d", code)
	}

	// Description newlines survive; CR and controls do not.
	code, m, raw := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"description":"line 1\r\nline 2\u0007"}`, room.ID, ok))
	if code != http.StatusCreated || m.Description != "line 1\nline 2" || m.TimeZone != "UTC" || m.DurationMin != 60 {
		t.Fatalf("defaults: %d %s", code, raw)
	}

	// Guest link off: invites for a scheduled meeting are refused; the view says so.
	if code, _ := h.do("POST", "/api/v1/rooms/"+room.ID+"/revoke", "", nil); code != 200 {
		t.Fatal(code)
	}
	code, m, _ = h.meetingCall("GET", "/api/v1/schedule/"+m.ID, "")
	if code != 200 || m.LinkReady || m.Invite.GuestURL != "" || !strings.Contains(m.Invite.Text, "The host will send you the link") {
		t.Fatalf("link off view: %+v", m)
	}
	if code, raw := h.do("GET", m.Invite.ICSPath, "", nil); code != http.StatusConflict {
		t.Fatalf("ics without link: %d %s", code, raw)
	}
	// A cancellation can always be sent.
	h.meetingCall("POST", "/api/v1/schedule/"+m.ID+"/cancel", "")
	if code, _ := h.do("GET", m.Invite.ICSPath, "", nil); code != 200 {
		t.Fatalf("cancel ics without link: %d", code)
	}

	// Archived room: no new meetings in it.
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+room.ID, `{"archived":true}`, nil); code != 200 {
		t.Fatal(code)
	}
	if code, _, raw := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, ok)); code != http.StatusConflict {
		t.Fatalf("archived room: %d %s", code, raw)
	}
	// Deleting the room deletes its meetings.
	other := h.createRoom("Other")
	_, m2, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, other.ID, ok))
	if code, _ := h.do("DELETE", "/api/v1/rooms/"+other.ID, "", nil); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if code, _ := h.do("GET", "/api/v1/schedule/"+m2.ID, "", nil); code != http.StatusNotFound {
		t.Fatalf("meeting outlived its room: %d", code)
	}
}

// Scheduling is host management: the admin credential on loopback, never
// a host token alone, never another position.
func TestScheduleIsHostOnly(t *testing.T) {
	h := newSecHarness(t)
	room := h.createRoom("Show")
	_, m, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, startIn(time.Hour)))
	start := h.start(room.ID)
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/schedule"}, {"POST", "/api/v1/schedule"}, {"GET", "/api/v1/schedule/" + m.ID},
		{"PATCH", "/api/v1/schedule/" + m.ID}, {"DELETE", "/api/v1/schedule/" + m.ID},
		{"POST", "/api/v1/schedule/" + m.ID + "/cancel"}, {"GET", "/api/v1/schedule/" + m.ID + "/invite.ics"},
		{"GET", "/api/v1/calendar/feed"}, {"POST", "/api/v1/calendar/feed/rotate"}, {"POST", "/api/v1/calendar/feed/revoke"},
	}
	guestTok := secretOf(t, room)
	for _, rt := range routes {
		for name, hdr := range map[string]http.Header{
			"no credential":   nil,
			"host token":      {"Authorization": {"Bearer " + start.HostToken}},
			"guest link":      {"Authorization": {"Bearer " + guestTok}},
			"wrong desktop":   {"Authorization": {"Bearer " + testDesktopToken + "x"}},
			"session minted":  {"Authorization": {"Bearer " + signaling.MintHostToken("x", time.Hour, h.cfg.SecretKey)}},
			"funnel + token":  {"Authorization": {"Bearer " + testDesktopToken}, "Tailscale-Funnel-Request": {"?1"}},
			"serve, no owner": {"Authorization": {"Bearer " + testDesktopToken}, "X-Forwarded-For": {"100.101.1.2"}},
		} {
			if resp := h.rawDo(rt.method, rt.path, hdr); resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s %s (%s) = %d, want 403", rt.method, rt.path, name, resp.StatusCode)
			}
		}
		// Through private Serve from a tailnet device without the owner login.
		if w := h.serve(rt.method, "https://studio.tail1234.ts.net"+rt.path, http.Header{"Authorization": {"Bearer " + testDesktopToken}, "X-Forwarded-For": {"100.101.1.2"}}); w.Code != http.StatusForbidden {
			t.Errorf("%s %s via Serve = %d", rt.method, rt.path, w.Code)
		}
	}
	// The launch cookie (browser dashboard) works.
	resp := h.rawDo("GET", "/?launch="+testLaunchToken, nil)
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		cookie = c
	}
	if cookie == nil {
		t.Fatal("no admin cookie")
	}
	if resp := h.rawDo("GET", "/api/v1/schedule", http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}}); resp.StatusCode != 200 {
		t.Fatalf("dashboard cookie: %d", resp.StatusCode)
	}
}

func TestCalendarFeedTailnetOnlyRotatableNeverFunnel(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	_, keep, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"title":"Episode 12"}`, room.ID, startIn(24*time.Hour)))
	_, gone, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"title":"Dropped"}`, room.ID, startIn(30*time.Hour)))
	h.meetingCall("POST", "/api/v1/schedule/"+gone.ID+"/cancel", "")

	var feed CalendarFeedView
	_, raw := h.do("GET", "/api/v1/calendar/feed", "", nil)
	_ = json.Unmarshal(raw, &feed)
	if feed.Enabled || feed.URL != "" {
		t.Fatalf("feed is off until turned on: %s", raw)
	}
	_, raw = h.do("POST", "/api/v1/calendar/feed/rotate", "", nil)
	_ = json.Unmarshal(raw, &feed)
	if !feed.Enabled || !feed.Remote || !strings.HasPrefix(feed.URL, "https://studio.tail1234.ts.net/api/v1/calendar/feed.ics?key=") ||
		!strings.HasPrefix(feed.WebcalURL, "webcal://studio.tail1234.ts.net/") || !strings.Contains(feed.LocalURL, "127.0.0.1") {
		t.Fatalf("feed view: %+v", feed)
	}
	u, _ := url.Parse(feed.URL)
	key := u.Query().Get("key")
	if len(key) != 64 {
		t.Fatalf("feed key length %d", len(key))
	}
	path := u.RequestURI()

	// This Mac (direct loopback).
	w := h.serve("GET", "http://127.0.0.1:7443"+path, nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/calendar") || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("local feed: %d %v", w.Code, w.Header())
	}
	body := w.Body.String()
	lines := unfoldICS(body)
	if icsProp(lines, "METHOD") != "" || icsProp(lines, "X-WR-CALNAME") != "p2p.video meetings" || !strings.Contains(body, "SUMMARY:Episode 12") ||
		strings.Contains(body, "Dropped") || icsProp(lines, "UID") != keep.UID || icsProp(lines, "URL") != room.GuestURL {
		t.Fatalf("feed body:\n%s", body)
	}
	// A tailnet device through private Serve (no owner login needed: the key is the credential).
	if w := h.serve("GET", "https://studio.tail1234.ts.net"+path, http.Header{"X-Forwarded-For": {"100.101.1.2"}, "Tailscale-User-Login": {"someone@example.com"}}); w.Code != 200 {
		t.Fatalf("tailnet feed: %d", w.Code)
	}
	if w := h.serve("HEAD", "https://studio.tail1234.ts.net"+path, http.Header{"X-Forwarded-For": {"fd7a:115c:a1e0::5"}}); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("tailnet HEAD: %d", w.Code)
	}
	// Never through Funnel, even with the right key; never from outside the tailnet.
	for name, hdr := range map[string]http.Header{
		"funnel":          {"Tailscale-Funnel-Request": {"?1"}, "X-Forwarded-For": {"203.0.113.9"}},
		"funnel, tailnet": {"Tailscale-Funnel-Request": {"?1"}, "X-Forwarded-For": {"100.101.1.2"}},
		"public proxy":    {"X-Forwarded-For": {"203.0.113.9"}},
		"no client":       nil,
	} {
		if w := h.serve("GET", "https://studio.tail1234.ts.net"+path, hdr); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://192.168.1.20:7443"+path, nil)
	r.RemoteAddr = "192.168.1.30:5000"
	rw := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rw, r)
	if rw.Code != http.StatusForbidden {
		t.Fatalf("LAN client: %d", rw.Code)
	}
	// Wrong key, rotated key, revoked feed.
	if w := h.serve("GET", "http://127.0.0.1:7443/api/v1/calendar/feed.ics?key="+strings.Repeat("0", 64), nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d", w.Code)
	}
	if w := h.serve("GET", "http://127.0.0.1:7443/api/v1/calendar/feed.ics", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", w.Code)
	}
	h.do("POST", "/api/v1/calendar/feed/rotate", "", nil)
	if w := h.serve("GET", "http://127.0.0.1:7443"+path, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("rotated key still works: %d", w.Code)
	}
	_, raw = h.do("POST", "/api/v1/calendar/feed/revoke", "", nil)
	feed = CalendarFeedView{}
	_ = json.Unmarshal(raw, &feed)
	if feed.Enabled || feed.URL != "" {
		t.Fatalf("revoked view: %s", raw)
	}
	if strings.Contains(logs.String(), key) || strings.Contains(logs.String(), "key=") {
		t.Fatalf("feed key logged: %s", logs.String())
	}
}

func TestDesktopCalendarMatchingDataHasNoSecrets(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	_, m, _ := h.schedule(fmt.Sprintf(`{"roomId":%q,"startsAt":%q,"title":"Episode 12","guestEmails":["ada@example.com"]}`, room.ID, startIn(2*time.Hour)))
	code, raw := h.do("GET", "/api/v1/desktop/calendar", "", nil)
	if code != 200 {
		t.Fatalf("desktop calendar: %d %s", code, raw)
	}
	var dc DesktopCalendar
	if err := json.Unmarshal(raw, &dc); err != nil {
		t.Fatal(err)
	}
	if len(dc.Rooms) != 1 || dc.Rooms[0].Slug != "the-podcast" || dc.Rooms[0].ID != room.ID {
		t.Fatalf("rooms: %+v", dc.Rooms)
	}
	if len(dc.Scheduled) != 1 || dc.Scheduled[0].ID != m.ID || dc.Scheduled[0].UID != m.UID || !dc.Scheduled[0].EndsAt.Equal(m.EndsAt) {
		t.Fatalf("scheduled: %+v", dc.Scheduled)
	}
	found := false
	for _, o := range dc.GuestOrigins {
		found = found || o == "https://studio.tail1234.ts.net"
	}
	if !found {
		t.Fatalf("origins: %v", dc.GuestOrigins)
	}
	for _, bad := range []string{secretOf(t, room), "token", "ada@example.com", "hostToken"} {
		if strings.Contains(string(raw), bad) {
			t.Fatalf("desktop calendar leaks %q: %s", bad, raw)
		}
	}
	// Desktop token only, loopback only, never Funnel or a browser.
	for name, hdr := range map[string]http.Header{
		"no token":     nil,
		"cookie-ish":   {"Authorization": {"Bearer nope"}},
		"funnel":       {"Authorization": {"Bearer " + testDesktopToken}, "Tailscale-Funnel-Request": {"?1"}},
		"browser":      {"Authorization": {"Bearer " + testDesktopToken}, "Origin": {h.ts.URL}},
		"serve header": {"Authorization": {"Bearer " + testDesktopToken}, "Tailscale-User-Login": {"me@example.com"}},
	} {
		if resp := h.rawDo("GET", "/api/v1/desktop/calendar", hdr); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if w := h.serve("GET", "https://studio.tail1234.ts.net/api/v1/desktop/calendar", http.Header{"Authorization": {"Bearer " + testDesktopToken}}); w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback host: %d", w.Code)
	}
}
