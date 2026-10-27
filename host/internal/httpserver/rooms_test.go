package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type roomsHarness struct {
	t   *testing.T
	srv *Server
	ts  *httptest.Server
	st  *store.Store
	cfg *config.Config
}

func newRoomsHarness(t *testing.T) *roomsHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{DataDir: t.TempDir(), PublicURL: "https://studio.tail1234.ts.net", SecretKey: []byte("rooms-test-host-secret-0123456789abc")}
	hub := signaling.NewHub(st, cfg, nil)
	t.Cleanup(hub.Close)
	srv := &Server{Config: cfg, Store: st, Hub: hub, DesktopToken: testDesktopToken}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &roomsHarness{t: t, srv: srv, ts: ts, st: st, cfg: cfg}
}

// do sends a loopback request (management allowed) unless headers say otherwise.
func (h *roomsHarness) do(method, path, body string, hdr http.Header) (int, []byte) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+path, strings.NewReader(body))
	// The local host app's credential unless the test sets its own.
	req.Header.Set("Authorization", "Bearer "+testDesktopToken)
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (h *roomsHarness) createRoom(name string) RoomView {
	h.t.Helper()
	code, raw := h.do("POST", "/api/v1/rooms", fmt.Sprintf(`{"name":%q}`, name), nil)
	if code != http.StatusCreated {
		h.t.Fatalf("create room: %d %s", code, raw)
	}
	var v RoomView
	_ = json.Unmarshal(raw, &v)
	return v
}

func (h *roomsHarness) start(roomID string) RoomStartResponse {
	h.t.Helper()
	code, raw := h.do("POST", "/api/v1/rooms/"+roomID+"/start", `{"hostName":"Ben"}`, nil)
	if code != 200 {
		h.t.Fatalf("start: %d %s", code, raw)
	}
	var r RoomStartResponse
	_ = json.Unmarshal(raw, &r)
	return r
}

func (h *roomsHarness) lobby(slug, token string) (int, RoomLobby) {
	h.t.Helper()
	code, raw := h.do("GET", "/api/v1/rooms/lobby?room="+url.QueryEscape(slug), "", http.Header{"Authorization": {"Bearer " + token}})
	var l RoomLobby
	_ = json.Unmarshal(raw, &l)
	return code, l
}

func (h *roomsHarness) dial(sessionID, token string, hdr http.Header) (*websocket.Conn, int) {
	h.t.Helper()
	u := "ws" + strings.TrimPrefix(h.ts.URL, "http") + "/api/v1/ws?" + url.Values{"session": {sessionID}, "token": {token}}.Encode()
	c, resp, err := websocket.DefaultDialer.Dial(u, hdr)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		return nil, code
	}
	h.t.Cleanup(func() { c.Close() })
	return c, 101
}

func hello(t *testing.T, c *websocket.Conn, name, role string) {
	t.Helper()
	raw, _ := json.Marshal(signaling.HelloPayload{Name: name, Role: role})
	if err := c.WriteJSON(signaling.Envelope{Type: signaling.TypeHello, Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

// next reads until an envelope of type typ arrives.
func next(t *testing.T, c *websocket.Conn, typ string) json.RawMessage {
	t.Helper()
	for {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		var env signaling.Envelope
		if err := c.ReadJSON(&env); err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		if env.Type == typ {
			return env.Payload
		}
	}
}

func admission(t *testing.T, c *websocket.Conn) signaling.AdmissionPayload {
	t.Helper()
	var adm signaling.AdmissionPayload
	_ = json.Unmarshal(next(t, c, signaling.TypeAdmission), &adm)
	return adm
}

func secretOf(t *testing.T, v RoomView) string {
	t.Helper()
	u, err := url.Parse(v.GuestURL)
	if err != nil || v.GuestURL == "" {
		t.Fatalf("no guest URL: %+v", v)
	}
	if !strings.HasPrefix(v.GuestURL, "https://studio.tail1234.ts.net/join?") || u.Query().Get("room") != v.Slug {
		t.Fatalf("guest URL shape: %s", v.GuestURL)
	}
	if v.GuestPath == "" || !strings.HasSuffix(v.GuestURL, v.GuestPath) {
		t.Fatalf("guest path: %+v", v)
	}
	return u.Query().Get("token")
}

func TestRoomCRUDRenameSettingsArchiveDelete(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	if room.Slug != "the-podcast" || !room.LinkEnabled || !room.GuestURLRemote || room.Live || room.Settings.AutoAdmit || room.Settings.LockOnStart {
		t.Fatalf("new room: %+v", room)
	}
	secret := secretOf(t, room)
	if len(secret) < 32 { // ≥128 bits of hex
		t.Fatalf("secret too short: %d", len(secret))
	}
	// Stored hashed: neither the database row nor any API field but the link
	// carries the secret.
	stored, _ := h.st.GetRoom(room.ID)
	if stored.InviteSecretHash == secret || stored.InviteNonce == secret || stored.InviteSecretHash != hashToken(secret) {
		t.Fatal("room secret must be stored only as its hash")
	}
	if code, raw := h.do("POST", "/api/v1/rooms", `{"name":""}`, nil); code != 400 {
		t.Fatalf("empty name: %d %s", code, raw)
	}
	if code, _ := h.do("POST", "/api/v1/rooms", `{"name":"a\u0000b"}`, nil); code != 400 {
		t.Fatalf("control char name: %d", code)
	}
	if code, _ := h.do("POST", "/api/v1/rooms", `{"name":"x","admin":true}`, nil); code != 400 {
		t.Fatalf("unknown field accepted: %d", code)
	}
	second := h.createRoom("The Podcast")
	if second.Slug != "the-podcast-2" || secretOf(t, second) == secret {
		t.Fatalf("second room: %+v", second)
	}

	code, raw := h.do("PATCH", "/api/v1/rooms/"+room.ID, `{"name":"The Show","settings":{"autoAdmit":true,"lockOnStart":true,"recording":{"target_fps":60}}}`, nil)
	if code != 200 {
		t.Fatalf("patch: %d %s", code, raw)
	}
	var patched RoomView
	_ = json.Unmarshal(raw, &patched)
	if patched.Name != "The Show" || patched.Slug != "the-podcast" || !patched.Settings.AutoAdmit || !patched.Settings.LockOnStart ||
		patched.Settings.Recording == nil || patched.Settings.Recording.TargetFPS != 60 || patched.Settings.Recording.VideoCodec != "h264" {
		t.Fatalf("patched: %+v", patched)
	}
	if secretOf(t, patched) != secret {
		t.Fatal("rename must keep the guest link")
	}

	code, raw = h.do("GET", "/api/v1/rooms", "", nil)
	var list struct{ Rooms []RoomView }
	_ = json.Unmarshal(raw, &list)
	if code != 200 || len(list.Rooms) != 2 || list.Rooms[0].Name != "The Podcast" || list.Rooms[1].Name != "The Show" {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+second.ID, `{"archived":true}`, nil); code != 200 {
		t.Fatalf("archive: %d", code)
	}
	_, raw = h.do("GET", "/api/v1/rooms", "", nil)
	_ = json.Unmarshal(raw, &list)
	if len(list.Rooms) != 1 {
		t.Fatalf("archived room listed: %+v", list.Rooms)
	}
	_, raw = h.do("GET", "/api/v1/rooms?archived=1", "", nil)
	_ = json.Unmarshal(raw, &list)
	if len(list.Rooms) != 2 {
		t.Fatalf("archived list: %+v", list.Rooms)
	}
	if code, _ := h.lobby(second.Slug, secretOf(t, second)); code != 401 {
		t.Fatalf("archived room lobby: %d", code)
	}
	if code, _ := h.do("POST", "/api/v1/rooms/"+second.ID+"/start", "", nil); code != 409 {
		t.Fatalf("start archived room: %d", code)
	}
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+second.ID, `{"archived":false}`, nil); code != 200 {
		t.Fatalf("unarchive: %d", code)
	}
	if code, _ := h.lobby(second.Slug, secretOf(t, second)); code != 200 {
		t.Fatalf("restored room lobby: %d", code)
	}
	if code, _ := h.do("DELETE", "/api/v1/rooms/"+second.ID, "", nil); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := h.do("GET", "/api/v1/rooms/"+second.ID, "", nil); code != 404 {
		t.Fatalf("deleted room still readable: %d", code)
	}
}

// The full room lifecycle: guests wait for the host, a meeting maps the stable
// link to the live session, ending it closes the call, the next meeting
// accepts the same link, and rotation retires the old link everywhere.
func TestRoomLinkMapsToLiveMeetingAcrossMeetingsAndRotation(t *testing.T) {
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h := newRoomsHarness(t)
	room := h.createRoom("Weekly sync")
	secret := secretOf(t, room)

	// Before the host starts: the link is valid, nothing is live.
	code, l := h.lobby(room.Slug, secret)
	if code != 200 || l.Live || l.SessionID != "" || l.Name != "Weekly sync" {
		t.Fatalf("lobby before start: %d %+v", code, l)
	}
	for _, bad := range []string{"", "nope", secret + "0", strings.ToUpper(secret)} {
		if code, _ := h.lobby(room.Slug, bad); code != 401 {
			t.Fatalf("lobby with bad secret %q: %d", bad, code)
		}
	}
	if code, _ := h.lobby("not-a-room", secret); code != 401 {
		t.Fatalf("unknown slug: %d", code)
	}

	// Host opens the room: a session linked to the room exists, but guests
	// still wait until the host is actually in the call.
	m1 := h.start(room.ID)
	if m1.Resumed || m1.SessionID == "" || m1.Title != "Weekly sync" {
		t.Fatalf("start: %+v", m1)
	}
	sess, _ := h.st.GetSession(m1.SessionID)
	if sess.RoomID != room.ID || sess.AutoAdmit {
		t.Fatalf("meeting session: %+v", sess)
	}
	if again := h.start(room.ID); !again.Resumed || again.SessionID != m1.SessionID {
		t.Fatalf("second start must rejoin the open meeting: %+v", again)
	}
	if _, l = h.lobby(room.Slug, secret); l.Live || l.HostName != "Ben" {
		t.Fatalf("lobby before host connects: %+v", l)
	}
	host, code := h.dial(m1.SessionID, m1.HostToken, nil)
	if code != 101 {
		t.Fatalf("host dial: %d", code)
	}
	hello(t, host, "Ben", "host")
	if adm := admission(t, host); adm.Status != "admitted" || adm.Role != "host" {
		t.Fatalf("host: %+v", adm)
	}
	if _, l = h.lobby(room.Slug, secret); !l.Live || l.SessionID != m1.SessionID {
		t.Fatalf("lobby after host joins: %+v", l)
	}

	// A guest with the room link gets the waiting room only.
	guest, code := h.dial(m1.SessionID, secret, nil)
	if code != 101 {
		t.Fatalf("guest dial with room link: %d", code)
	}
	hello(t, guest, "Ada", "host") // a claimed role is ignored
	pending := admission(t, guest)
	if pending.Status != "pending" || pending.Role == "host" || pending.UploadToken != "" || len(pending.IceServers) != 0 {
		t.Fatalf("room link must grant the waiting room only: %+v", pending)
	}
	// The room secret is not a host credential.
	if code, _ := h.do("POST", "/api/v1/sessions/"+m1.SessionID+"/end", "", http.Header{"Authorization": {"Bearer " + secret}}); code != 401 {
		t.Fatalf("room secret ended the meeting: %d", code)
	}
	if code, _ := h.do("GET", "/api/v1/sessions/"+m1.SessionID, "", http.Header{"Authorization": {"Bearer " + secret}}); code != 200 {
		t.Fatalf("guest session detail with room link: %d", code)
	}
	raw, _ := json.Marshal(signaling.AdmitPayload{ParticipantID: pending.ParticipantID})
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeAdmit, Payload: raw})
	if adm := admission(t, guest); adm.Status != "admitted" || adm.UploadToken == "" {
		t.Fatalf("admit: %+v", adm)
	}

	// Another room's link cannot reach this meeting.
	other := h.createRoom("Other")
	if _, code := h.dial(m1.SessionID, secretOf(t, other), nil); code != 401 {
		t.Fatalf("other room's link joined: %d", code)
	}

	// Ending: refused while recording, then everyone is told and dropped.
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeRecordStart})
	waitRecording(t, host, true)
	if code, _ := h.do("POST", "/api/v1/sessions/"+m1.SessionID+"/end", "", http.Header{"Authorization": {"Bearer " + m1.HostToken}}); code != 409 {
		t.Fatalf("end while recording: %d", code)
	}
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeRecordStop})
	waitRecording(t, host, false)
	if code, raw := h.do("POST", "/api/v1/sessions/"+m1.SessionID+"/end", "", http.Header{"Authorization": {"Bearer " + m1.HostToken}}); code != 200 {
		t.Fatalf("end: %d %s", code, raw)
	}
	for _, c := range []*websocket.Conn{host, guest} {
		var end signaling.SessionEndPayload
		_ = json.Unmarshal(next(t, c, signaling.TypeSessionEnd), &end)
		if end.Reason != signaling.SessionEndByHost {
			t.Fatalf("session_end: %+v", end)
		}
	}
	if _, l = h.lobby(room.Slug, secret); l.Live {
		t.Fatalf("ended meeting still live: %+v", l)
	}
	if _, code := h.dial(m1.SessionID, secret, nil); code != 401 {
		t.Fatalf("ended meeting accepted the room link: %d", code)
	}
	if code, _ := h.do("POST", "/api/v1/sessions/"+m1.SessionID+"/end", "", http.Header{"Authorization": {"Bearer " + m1.HostToken}}); code != 200 {
		t.Fatalf("end is idempotent: %d", code)
	}

	// Second meeting, same link.
	m2 := h.start(room.ID)
	if m2.Resumed || m2.SessionID == m1.SessionID {
		t.Fatalf("second meeting: %+v", m2)
	}
	host2, _ := h.dial(m2.SessionID, m2.HostToken, nil)
	hello(t, host2, "Ben", "host")
	admission(t, host2)
	if _, l = h.lobby(room.Slug, secret); !l.Live || l.SessionID != m2.SessionID {
		t.Fatalf("lobby for second meeting: %+v", l)
	}
	g2, code := h.dial(m2.SessionID, secret, nil)
	if code != 101 {
		t.Fatalf("same link, second meeting: %d", code)
	}
	hello(t, g2, "Ada", "guest")
	if adm := admission(t, g2); adm.Status != "pending" {
		t.Fatalf("second meeting guest: %+v", adm)
	}

	// Rotate: the old link is refused everywhere, the new one works.
	code, raw = h.do("POST", "/api/v1/rooms/"+room.ID+"/rotate", "", nil)
	if code != 200 {
		t.Fatalf("rotate: %d", code)
	}
	var rotated RoomView
	_ = json.Unmarshal(raw, &rotated)
	fresh := secretOf(t, rotated)
	if fresh == secret {
		t.Fatal("rotation kept the secret")
	}
	if code, _ := h.lobby(room.Slug, secret); code != 401 {
		t.Fatalf("old link lobby after rotate: %d", code)
	}
	if _, code := h.dial(m2.SessionID, secret, nil); code != 401 {
		t.Fatalf("old link WS after rotate: %d", code)
	}
	if code, _ := h.do("GET", "/api/v1/sessions/"+m2.SessionID, "", http.Header{"Authorization": {"Bearer " + secret}}); code != 401 {
		t.Fatalf("old link session detail after rotate: %d", code)
	}
	if code, l := h.lobby(room.Slug, fresh); code != 200 || !l.Live {
		t.Fatalf("new link lobby: %d %+v", code, l)
	}
	if _, code := h.dial(m2.SessionID, fresh, nil); code != 101 {
		t.Fatalf("new link WS: %d", code)
	}

	// Revoke disables the link entirely; rotating issues a working one again.
	code, raw = h.do("POST", "/api/v1/rooms/"+room.ID+"/revoke", "", nil)
	var revoked RoomView
	_ = json.Unmarshal(raw, &revoked)
	if code != 200 || revoked.LinkEnabled || revoked.GuestURL != "" {
		t.Fatalf("revoke: %d %+v", code, revoked)
	}
	if code, _ := h.lobby(room.Slug, fresh); code != 401 {
		t.Fatalf("revoked link lobby: %d", code)
	}
	if _, code := h.dial(m2.SessionID, fresh, nil); code != 401 {
		t.Fatalf("revoked link WS: %d", code)
	}
	// Live rooms cannot be archived or deleted out from under a meeting.
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+room.ID, `{"archived":true}`, nil); code != 409 {
		t.Fatalf("archive live room: %d", code)
	}
	if code, _ := h.do("DELETE", "/api/v1/rooms/"+room.ID, "", nil); code != 409 {
		t.Fatalf("delete live room: %d", code)
	}

	// Never log link secrets.
	time.Sleep(50 * time.Millisecond)
	out := logs.String()
	for _, s := range []string{secret, fresh, m1.HostToken, m2.HostToken} {
		if strings.Contains(out, s) {
			t.Fatalf("a secret reached the logs")
		}
	}
	if !strings.Contains(out, "Room guest link rotated") {
		t.Fatalf("expected room audit lines in logs:\n%s", out)
	}
}

func TestRoomDefaultsApplyToMeetings(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("Open house")
	secret := secretOf(t, room)
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+room.ID, `{"settings":{"autoAdmit":true}}`, nil); code != 200 {
		t.Fatal(code)
	}
	m := h.start(room.ID)
	host, _ := h.dial(m.SessionID, m.HostToken, nil)
	hello(t, host, "Ben", "host")
	admission(t, host)
	g, _ := h.dial(m.SessionID, secret, nil)
	hello(t, g, "Ada", "guest")
	if adm := admission(t, g); adm.Status != "admitted" {
		t.Fatalf("auto-admit room: %+v", adm)
	}

	locked := h.createRoom("Locked")
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+locked.ID, `{"settings":{"lockOnStart":true}}`, nil); code != 200 {
		t.Fatal(code)
	}
	lm := h.start(locked.ID)
	lh, _ := h.dial(lm.SessionID, lm.HostToken, nil)
	hello(t, lh, "Ben", "host")
	if adm := admission(t, lh); adm.Status != "admitted" {
		t.Fatalf("host locked out: %+v", adm)
	}
	lg, _ := h.dial(lm.SessionID, secretOf(t, locked), nil)
	hello(t, lg, "Ada", "guest")
	if adm := admission(t, lg); adm.Status != "denied" || adm.Reason != signaling.ReasonRoomLocked {
		t.Fatalf("lock-on-start: %+v", adm)
	}
}

// Room management is host-only: never through Funnel, never from a remote
// address, not even with a valid host token; the guest lobby stays reachable.
func TestRoomManagementDeniedRemotelyAndThroughFunnel(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	secret := secretOf(t, room)
	m := h.start(room.ID)
	funnel := func(r *http.Request) {
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header.Set("Tailscale-Funnel-Request", "?1")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Header.Set("Origin", h.cfg.PublicURL)
	}
	remote := func(r *http.Request) { r.RemoteAddr = "100.100.1.2:40000" }
	cases := []struct{ method, path, body string }{
		{"GET", "/api/v1/rooms", ""},
		{"POST", "/api/v1/rooms", `{"name":"x"}`},
		{"GET", "/api/v1/rooms/" + room.ID, ""},
		{"PATCH", "/api/v1/rooms/" + room.ID, `{"name":"y"}`},
		{"DELETE", "/api/v1/rooms/" + room.ID, ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/rotate", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/revoke", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/start", ""},
		{"GET", "/api/v1/home", ""},
		{"POST", "/api/v1/sessions/" + m.SessionID + "/end", ""},
	}
	for name, via := range map[string]func(*http.Request){"funnel": funnel, "remote": remote} {
		for _, tc := range cases {
			r := httptest.NewRequest(tc.method, "https://studio.tail1234.ts.net"+tc.path, bytes.NewBufferString(tc.body))
			via(r)
			r.Header.Set("Authorization", "Bearer "+m.HostToken)
			w := httptest.NewRecorder()
			h.srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s via %s = %d, want denied", tc.method, tc.path, name, w.Code)
			}
			if strings.Contains(w.Body.String(), secret) {
				t.Errorf("%s %s via %s leaked the room link", tc.method, tc.path, name)
			}
		}
	}
	if got, _ := h.st.GetRoom(room.ID); got.Name != "The Podcast" || !got.LinkEnabled() {
		t.Fatalf("denied requests changed the room: %+v", got)
	}
	// The guest lobby works through Funnel with the link, and only with it.
	for _, tok := range []string{secret, m.HostToken} {
		r := httptest.NewRequest("GET", "https://studio.tail1234.ts.net/api/v1/rooms/lobby?room="+room.Slug, nil)
		funnel(r)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(w, r)
		want := 200
		if tok != secret {
			want = 401
		}
		if w.Code != want {
			t.Fatalf("lobby through Funnel with %s: %d", map[bool]string{true: "link", false: "host token"}[tok == secret], w.Code)
		}
		if strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), "slug") {
			t.Fatalf("lobby leaked room data: %s", w.Body.String())
		}
	}
}

func TestHomeListsRoomsAndRecentMeetings(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	m := h.start(room.ID)
	code, raw := h.do("POST", "/api/v1/sessions", `{"title":"Instant"}`, nil)
	if code != 200 {
		t.Fatal(code)
	}
	code, raw = h.do("GET", "/api/v1/home", "", nil)
	if code != 200 {
		t.Fatalf("home: %d", code)
	}
	var home struct {
		Rooms            []RoomView
		Recent           []HomeSession
		GuestLinksRemote bool
	}
	_ = json.Unmarshal(raw, &home)
	if len(home.Rooms) != 1 || !home.Rooms[0].Live || home.Rooms[0].LiveSessionID != m.SessionID || !home.GuestLinksRemote {
		t.Fatalf("home rooms: %+v", home.Rooms)
	}
	if len(home.Recent) != 2 || home.Recent[0].Title != "Instant" || home.Recent[1].RoomName != "The Podcast" {
		t.Fatalf("home recent: %+v", home.Recent)
	}
	for _, s := range home.Recent {
		if s.InviteTokenHash != "" || !signaling.VerifyHostToken(s.HostToken, s.ID, h.cfg.SecretKey) {
			t.Fatalf("recent session credentials: %+v", s)
		}
	}
	if strings.Contains(string(raw), "inviteSecretHash") || strings.Contains(string(raw), "inviteNonce") {
		t.Fatal("home exposes room link material")
	}
}

func waitRecording(t *testing.T, c *websocket.Conn, want bool) {
	t.Helper()
	for {
		var st signaling.RoomStatePayload
		_ = json.Unmarshal(next(t, c, signaling.TypeRoomState), &st)
		if st.Recording == want {
			return
		}
	}
}
