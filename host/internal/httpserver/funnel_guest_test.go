package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/integrations"
	"github.com/omniharmonic/studiolink/host/internal/livestream"
	"github.com/omniharmonic/studiolink/host/internal/parachute"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/turn"
)

// A public guest arrives through Tailscale Funnel (which sets
// Tailscale-Funnel-Request). Guest join + admission must work and deliver
// participant-bound TURN credentials, while every host-management path,
// including host control over the WebSocket, stays denied through Funnel.
func TestFunnelGuestJoinsWhileManagementStaysDenied(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	turnSecret := []byte("funnel-test-turn-secret-0123456789")
	cfg := &config.Config{
		DataDir:    t.TempDir(),
		PublicURL:  "https://studio.tail1234.ts.net",
		SecretKey:  []byte("funnel-test-host-secret-0123456789"),
		PublicSTUN: true, STUNURLs: []string{"stun:stun.l.google.com:19302"},
		TURNSecret: turnSecret, TURNURLs: []string{"turns:studio.tail1234.ts.net:8443?transport=tcp"},
	}
	hub := signaling.NewHub(st, cfg, nil)
	defer hub.Close()
	// A configured livestream manager, so Funnel denial is proven to be the
	// authorization check, not an unconfigured-feature 503.
	srv := &Server{Config: cfg, Store: st, Hub: hub, Livestream: livestream.NewManager((&recordingFFmpeg{}).launch, livestream.Options{}), DesktopToken: testDesktopToken}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Local owner creates the room and an invite (loopback, not Funnel).
	resp, err := adminPost(ts.URL+"/api/v1/sessions", `{"title":"Public","autoAdmit":false}`)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("local create: %v %v", err, resp)
	}
	var sess CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/api/v1/sessions/%s/invite?token=%s", ts.URL, sess.ID, sess.HostToken), nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("invite: %v %v", err, resp)
	}
	var inv map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&inv)

	// And a persistent room [SL-P1-5].
	resp, err = adminPost(ts.URL+"/api/v1/rooms", `{"name":"The Podcast"}`)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("local room create: %v %v", err, resp)
	}
	var room RoomView
	_ = json.NewDecoder(resp.Body).Decode(&room)
	// And an async record link [SL-P5-3].
	resp, err = adminPost(ts.URL+"/api/v1/recordlinks", `{"title":"Solo"}`)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("local record link create: %v %v", err, resp)
	}
	var recLink RecordLinkView
	_ = json.NewDecoder(resp.Body).Decode(&recLink)
	// And a scheduled meeting plus the host calendar feed [SL-P6-1].
	resp, err = adminPost(ts.URL+"/api/v1/schedule", fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("local schedule: %v %v", err, resp)
	}
	var meeting MeetingView
	_ = json.NewDecoder(resp.Body).Decode(&meeting)
	resp, err = adminPost(ts.URL+"/api/v1/calendar/feed/rotate", "")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("local feed: %v %v", err, resp)
	}
	var feed CalendarFeedView
	_ = json.NewDecoder(resp.Body).Decode(&feed)
	feedURL, _ := url.Parse(feed.URL)
	if feedURL == nil || feedURL.Query().Get("key") == "" {
		t.Fatalf("feed URL: %+v", feed)
	}

	funnel := func(h http.Header) http.Header {
		if h == nil {
			h = http.Header{}
		}
		h.Set("Tailscale-Funnel-Request", "?1")
		h.Set("X-Forwarded-For", "203.0.113.9")
		h.Set("Origin", cfg.PublicURL)
		return h
	}

	// A complete take with integrations configured, so the denials below
	// come from authorization, not from a missing take or service.
	const funnelTake = "funnel-take"
	if err := st.CreateTake(&store.Take{ID: funnelTake, SessionID: sess.ID, Number: 1, Status: "complete", WallClockStart: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv.Pipeline = postproc.NewPipeline(st, cfg, nil)
	srv.Integrations = &integrations.Service{Store: st, Package: srv.Pipeline, Parachute: &parachute.Client{Origin: "https://hub.invalid", Vault: "v"}}
	defer srv.Integrations.Close()

	// Management through Funnel is denied, even with a valid host token.
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/sessions", `{}`},
		{"POST", "/api/v1/sessions/" + sess.ID + "/settings", `{"target_fps":60}`},
		{"POST", "/api/v1/sessions/" + sess.ID + "/invite", ""},
		{"GET", "/api/v1/sessions/" + sess.ID + "/livestream", ""},
		{"POST", "/api/v1/sessions/" + sess.ID + "/livestream", `{"destinations":[{"name":"x","url":"rtmp://127.0.0.1/live","key":"k"}]}`},
		{"DELETE", "/api/v1/sessions/" + sess.ID + "/livestream", ""},
		{"GET", "/api/v1/sessions/" + sess.ID + "/livestream/ingest", ""},
		{"GET", "/api/v1/sessions/" + sess.ID + "/captions/ingest", ""},
		{"GET", "/api/v1/sessions/" + sess.ID + "/captions/transcript?format=vtt", ""},
		{"GET", "/api/v1/captions/provider", ""},
		{"GET", "/api/v1/doctor", ""},
		{"GET", "/api/v1/debug/runtime", ""},
		{"POST", "/api/v1/sessions/" + sess.ID + "/end", ""},
		{"GET", "/api/v1/home", ""},
		{"GET", "/api/v1/rooms", ""},
		{"POST", "/api/v1/rooms", `{"name":"x"}`},
		{"GET", "/api/v1/rooms/" + room.ID, ""},
		{"PATCH", "/api/v1/rooms/" + room.ID, `{"name":"y"}`},
		{"DELETE", "/api/v1/rooms/" + room.ID, ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/rotate", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/revoke", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/start", ""},
		// Producer links (SL-P1-11).
		{"GET", "/api/v1/sessions/" + sess.ID + "/producer", ""},
		{"POST", "/api/v1/sessions/" + sess.ID + "/producer/rotate", ""},
		{"POST", "/api/v1/sessions/" + sess.ID + "/producer/revoke", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/producer/rotate", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/producer/revoke", ""},
		// Branding and logo (SL-P5-2).
		{"GET", "/api/v1/sessions/" + sess.ID + "/branding", ""},
		{"PUT", "/api/v1/sessions/" + sess.ID + "/branding", `{"accent":"#000000"}`},
		{"GET", "/api/v1/sessions/" + sess.ID + "/branding/logo", ""},
		{"POST", "/api/v1/sessions/" + sess.ID + "/branding/logo", "\x89PNG\r\n\x1a\n"},
		{"DELETE", "/api/v1/sessions/" + sess.ID + "/branding/logo", ""},
		{"GET", "/api/v1/rooms/" + room.ID + "/branding", ""},
		{"PUT", "/api/v1/rooms/" + room.ID + "/branding", `{"accent":"#000000"}`},
		{"GET", "/api/v1/rooms/" + room.ID + "/branding/logo", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/branding/logo", "\x89PNG\r\n\x1a\n"},
		{"DELETE", "/api/v1/rooms/" + room.ID + "/branding/logo", ""},
		// Lower thirds a room remembers (SL-P5-4).
		{"GET", "/api/v1/rooms/" + room.ID + "/titles", ""},
		{"PUT", "/api/v1/rooms/" + room.ID + "/titles", `{"displayName":"a","title":"b"}`},
		{"DELETE", "/api/v1/rooms/" + room.ID + "/titles?key=a", ""},
		// Post-take ecosystem actions (SL-P4-1), on a real take with the service configured.
		{"GET", "/api/v1/takes/" + funnelTake + "/integrations", ""},
		{"POST", "/api/v1/takes/" + funnelTake + "/transcript", ""},
		// Built-in transcription (SL-P4-3): cancel, engine choice, provider socket.
		{"POST", "/api/v1/takes/" + funnelTake + "/transcript/cancel", ""},
		{"PUT", "/api/v1/takes/" + funnelTake + "/transcript/engine", `{"engine":"builtin"}`},
		{"GET", "/api/v1/transcription/provider", ""},
		{"POST", "/api/v1/takes/" + funnelTake + "/parachute", ""},
		{"POST", "/api/v1/takes/" + funnelTake + "/handoff/clip-editor", ""},
		{"GET", "/api/v1/takes/" + funnelTake + "/download", ""},
		// Quick-delivery originals and export progress (SL-P0-9).
		{"GET", "/api/v1/takes/" + funnelTake + "/originals", ""},
		{"GET", "/api/v1/takes/" + funnelTake, ""},
		{"POST", "/api/v1/takes/" + funnelTake + "/postprocess", ""},
		// Async record links (SL-P5-3): management only; the guest side is
		// /api/v1/record/ (TestRecordLinkFunnel).
		{"GET", "/api/v1/recordlinks", ""},
		{"POST", "/api/v1/recordlinks", `{"title":"x"}`},
		{"GET", "/api/v1/recordlinks/" + recLink.ID, ""},
		{"PATCH", "/api/v1/recordlinks/" + recLink.ID, `{"title":"y"}`},
		{"DELETE", "/api/v1/recordlinks/" + recLink.ID, ""},
		{"POST", "/api/v1/recordlinks/" + recLink.ID + "/rotate", ""},
		{"POST", "/api/v1/recordlinks/" + recLink.ID + "/revoke", ""},
		{"DELETE", "/api/v1/recordlinks/" + recLink.ID + "/submissions/" + sess.ID, ""},
		// Abandoned drafts (SL-P5-4).
		{"GET", "/api/v1/recordlinks/drafts", ""},
		{"POST", "/api/v1/recordlinks/drafts/cleanup", "{}"},
		{"DELETE", "/api/v1/recordlinks/drafts/" + sess.ID, ""},
		// Scheduling, invites and the calendar feed (SL-P6-1): management
		// only, and the feed itself never through Funnel, even with its key.
		{"GET", "/api/v1/schedule", ""},
		{"POST", "/api/v1/schedule", fmt.Sprintf(`{"roomId":%q,"startsAt":%q}`, room.ID, meeting.StartsAt.Format(time.RFC3339))},
		{"GET", "/api/v1/schedule/" + meeting.ID, ""},
		{"PATCH", "/api/v1/schedule/" + meeting.ID, `{"title":"y"}`},
		{"DELETE", "/api/v1/schedule/" + meeting.ID, ""},
		{"POST", "/api/v1/schedule/" + meeting.ID + "/cancel", ""},
		{"GET", "/api/v1/schedule/" + meeting.ID + "/invite.ics", ""},
		{"GET", "/api/v1/calendar/feed", ""},
		{"POST", "/api/v1/calendar/feed/rotate", ""},
		{"POST", "/api/v1/calendar/feed/revoke", ""},
		{"GET", feedURL.RequestURI(), ""},
		{"HEAD", feedURL.RequestURI(), ""},
	} {
		r := httptest.NewRequest(tc.method, "https://studio.tail1234.ts.net"+tc.path, bytes.NewBufferString(tc.body))
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header = funnel(r.Header)
		r.Header.Set("Authorization", "Bearer "+sess.HostToken)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s through Funnel = %d, want denied", tc.method, tc.path, w.Code)
		}
	}
	// The desktop app's state and calendar endpoints are denied through Funnel even with its token.
	for _, host := range []string{"https://studio.tail1234.ts.net/api/v1/desktop/state", "http://127.0.0.1:7443/api/v1/desktop/state",
		"https://studio.tail1234.ts.net/api/v1/desktop/calendar", "http://127.0.0.1:7443/api/v1/desktop/calendar"} {
		r := httptest.NewRequest("GET", host, nil)
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header = funnel(r.Header)
		r.Header.Del("Origin")
		r.Header.Set("Authorization", "Bearer "+srv.DesktopToken)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("desktop endpoint through Funnel (%s) = %d, want 403", host, w.Code)
		}
	}
	// The same feed key works on this Mac: the Funnel denial above is the guardrail, not a bad key.
	if r, err := http.Get(ts.URL + feedURL.RequestURI()); err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("feed on loopback: %v %v", err, r)
	}
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
	if c, resp, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s", wsURL, sess.ID, sess.HostToken), funnel(nil)); err == nil {
		c.Close()
		t.Fatal("host control WebSocket accepted through Funnel")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("host WS through Funnel: %v", err)
	}
	// The livestream ingest socket is host management too.
	ingestURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/sessions/" + sess.ID + "/livestream/ingest"
	if c, resp, err := websocket.DefaultDialer.Dial(ingestURL+"?token="+sess.HostToken, funnel(nil)); err == nil {
		c.Close()
		t.Fatal("livestream ingest accepted through Funnel")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("livestream ingest through Funnel: %v", err)
	}

	// Local host connects; Funnel guest joins and waits in the lobby.
	host, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s", wsURL, sess.ID, sess.HostToken), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	hello := func(c *websocket.Conn, role string) {
		raw, _ := json.Marshal(signaling.HelloPayload{Name: role, Role: role})
		_ = c.WriteJSON(signaling.Envelope{Type: signaling.TypeHello, Payload: raw})
	}
	hello(host, "host")
	guest, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s", wsURL, sess.ID, inv["token"]), funnel(nil))
	if err != nil {
		t.Fatalf("Funnel guest join rejected: %v", err)
	}
	defer guest.Close()
	hello(guest, "guest")

	readAdm := func() signaling.AdmissionPayload {
		t.Helper()
		for {
			_ = guest.SetReadDeadline(time.Now().Add(3 * time.Second))
			var env signaling.Envelope
			if err := guest.ReadJSON(&env); err != nil {
				t.Fatal(err)
			}
			if env.Type == signaling.TypeAdmission {
				var adm signaling.AdmissionPayload
				_ = json.Unmarshal(env.Payload, &adm)
				return adm
			}
		}
	}
	pending := readAdm()
	if pending.Status != "pending" || len(pending.IceServers) != 0 {
		t.Fatalf("pending Funnel guest got ICE servers: %+v", pending)
	}
	raw, _ := json.Marshal(signaling.AdmitPayload{ParticipantID: pending.ParticipantID})
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeAdmit, Payload: raw})
	adm := readAdm()
	if adm.Status != "admitted" || adm.UploadToken == "" {
		t.Fatalf("Funnel guest not admitted: %+v", adm)
	}
	var sawSTUN, sawTURN bool
	for _, s := range adm.IceServers {
		for _, u := range s.URLs {
			if strings.HasPrefix(u, "stun:") {
				sawSTUN = true
			}
			if u == "turns:studio.tail1234.ts.net:8443?transport=tcp" {
				sawTURN = true
				if pid, err := turn.Verify(turnSecret, s.Username, s.Credential, time.Now()); err != nil || pid != adm.ParticipantID {
					t.Fatalf("TURN credential not bound to admitted guest: %v", err)
				}
			}
		}
	}
	if !sawSTUN || !sawTURN {
		t.Fatalf("admission ICE servers incomplete: %+v", adm.IceServers)
	}
}
