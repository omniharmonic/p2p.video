package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

// Producer role [SL-P1-11]: link issuance and rotation, the permission
// matrix of every host-only / host-assist signaling message for host vs
// producer vs guest, recording exclusion, and every host HTTP route refusing
// producer credentials.

func (h *roomsHarness) instantSession() CreateSessionResponse {
	h.t.Helper()
	code, raw := h.do("POST", "/api/v1/sessions", `{"title":"Episode"}`, nil)
	if code != 200 {
		h.t.Fatalf("create session: %d %s", code, raw)
	}
	var s CreateSessionResponse
	_ = json.Unmarshal(raw, &s)
	return s
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }

func (h *roomsHarness) producerLink(sessionID, hostToken, action string) signaling.ProducerLinkResponse {
	h.t.Helper()
	method, path := "GET", "/api/v1/sessions/"+sessionID+"/producer"
	if action != "" {
		method, path = "POST", path+"/"+action
	}
	code, raw := h.do(method, path, "", bearer(hostToken))
	if code != 200 {
		h.t.Fatalf("%s %s: %d %s", method, path, code, raw)
	}
	var v signaling.ProducerLinkResponse
	_ = json.Unmarshal(raw, &v)
	return v
}

func linkToken(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil || u.Query().Get("token") == "" {
		t.Fatalf("link without token: %q", raw)
	}
	return u.Query().Get("token")
}

// sendEnv writes one envelope.
func sendEnv(t *testing.T, c *websocket.Conn, typ string, payload any) {
	t.Helper()
	var raw json.RawMessage
	if payload != nil {
		raw, _ = json.Marshal(payload)
	}
	if err := c.WriteJSON(signaling.Envelope{Type: typ, Payload: raw}); err != nil {
		t.Fatal(err)
	}
}

// until reads envelopes until match returns true, returning every type seen.
func until(t *testing.T, c *websocket.Conn, what string, match func(signaling.Envelope) bool) []string {
	t.Helper()
	var seen []string
	for {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		var env signaling.Envelope
		if err := c.ReadJSON(&env); err != nil {
			t.Fatalf("waiting for %s (saw %v): %v", what, seen, err)
		}
		seen = append(seen, env.Type)
		if match(env) {
			return seen
		}
	}
}

// forbidden expects an action_error forbidden for action.
func forbidden(t *testing.T, c *websocket.Conn, action string) {
	t.Helper()
	until(t, c, "forbidden "+action, func(env signaling.Envelope) bool {
		if env.Type != signaling.TypeActionError {
			return false
		}
		var e signaling.ActionErrorPayload
		_ = json.Unmarshal(env.Payload, &e)
		if e.Action != action {
			return false
		}
		if e.Code != signaling.ErrCodeForbidden {
			t.Fatalf("%s: code %q, want forbidden", action, e.Code)
		}
		return true
	})
}

// silentFor reads for d and fails if an envelope of typ arrives.
func silentFor(t *testing.T, c *websocket.Conn, typ string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		_ = c.SetReadDeadline(deadline)
		var env signaling.Envelope
		if err := c.ReadJSON(&env); err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				break
			}
			if strings.Contains(err.Error(), "timeout") {
				break
			}
			t.Fatalf("reading while expecting no %s: %v", typ, err)
		}
		if env.Type == typ {
			t.Fatalf("unexpected %s", typ)
		}
	}
}

// admissionWith reads admissions until one with status arrives.
func admissionWith(t *testing.T, c *websocket.Conn, status string) signaling.AdmissionPayload {
	t.Helper()
	var adm signaling.AdmissionPayload
	until(t, c, "admission "+status, func(env signaling.Envelope) bool {
		if env.Type != signaling.TypeAdmission {
			return false
		}
		adm = signaling.AdmissionPayload{}
		_ = json.Unmarshal(env.Payload, &adm)
		return adm.Status == status
	})
	return adm
}

func joinRequestFor(t *testing.T, c *websocket.Conn, role string) string {
	t.Helper()
	var id string
	until(t, c, "join_request "+role, func(env signaling.Envelope) bool {
		if env.Type != signaling.TypeJoinRequest {
			return false
		}
		var p map[string]string
		_ = json.Unmarshal(env.Payload, &p)
		if p["role"] != role {
			return false
		}
		id = p["participantId"]
		return true
	})
	return id
}

func TestProducerLinkIssueRotateRevoke(t *testing.T) {
	h := newRoomsHarness(t)
	s := h.instantSession()
	if v := h.producerLink(s.ID, s.HostToken, ""); v.Enabled || v.URL != "" {
		t.Fatalf("producer link before issue: %+v", v)
	}
	v := h.producerLink(s.ID, s.HostToken, "rotate")
	if !v.Enabled || !strings.HasPrefix(v.URL, "https://studio.tail1234.ts.net/join?") || !v.Remote {
		t.Fatalf("issued link: %+v", v)
	}
	tok := linkToken(t, v.URL)
	if tok == s.Token || len(tok) < 32 {
		t.Fatal("producer link must be its own 256-bit secret")
	}
	if again := h.producerLink(s.ID, s.HostToken, ""); again.URL != v.URL {
		t.Fatal("GET must show the same link again")
	}
	if nonce, hash, _ := h.st.ProducerLink(store.SessionProducerScope(s.ID)); hash != hashToken(tok) || nonce == tok || hash == tok {
		t.Fatal("producer secret must be stored only as its hash")
	}
	// The link joins as producer and waits for the host.
	host, _ := h.dial(s.ID, s.HostToken, nil)
	hello(t, host, "Host", "host")
	admission(t, host)
	p, code := h.dial(s.ID, tok, nil)
	if code != 101 {
		t.Fatalf("producer link refused: %d", code)
	}
	hello(t, p, "Pat", "guest") // the claimed role is ignored
	if adm := admission(t, p); adm.Status != "pending" {
		t.Fatalf("producer not waiting: %+v", adm)
	}
	joinRequestFor(t, host, signaling.RoleProducer)
	// Session detail with the producer link says so (green room).
	code, raw := h.do("GET", "/api/v1/sessions/"+s.ID, "", bearer(tok))
	if code != 200 || !strings.Contains(string(raw), `"joinRole":"producer"`) {
		t.Fatalf("detail with producer link: %d %s", code, raw)
	}
	if _, raw := h.do("GET", "/api/v1/sessions/"+s.ID, "", bearer(s.Token)); strings.Contains(string(raw), "joinRole") {
		t.Fatal("a guest invite must not read as a producer link")
	}

	// Rotate: the old link stops working at once, and whoever waits under it is denied.
	v2 := h.producerLink(s.ID, s.HostToken, "rotate")
	if v2.URL == v.URL {
		t.Fatal("rotate kept the link")
	}
	if adm := admission(t, p); adm.Status != "denied" {
		t.Fatalf("waiting producer under the old link: %+v", adm)
	}
	if _, code := h.dial(s.ID, tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("old producer link after rotate: %d", code)
	}
	if code, _ := h.do("GET", "/api/v1/sessions/"+s.ID, "", bearer(tok)); code != 401 {
		t.Fatalf("old producer link detail: %d", code)
	}
	if _, code := h.dial(s.ID, linkToken(t, v2.URL), nil); code != 101 {
		t.Fatalf("new producer link: %d", code)
	}
	// Revoke: no link at all.
	if v3 := h.producerLink(s.ID, s.HostToken, "revoke"); v3.Enabled || v3.URL != "" {
		t.Fatalf("revoked link: %+v", v3)
	}
	if _, code := h.dial(s.ID, linkToken(t, v2.URL), nil); code != http.StatusUnauthorized {
		t.Fatalf("revoked producer link: %d", code)
	}
	// Only the host manages it.
	for _, tok := range []string{s.Token, linkToken(t, v2.URL), ""} {
		if code, _ := h.do("POST", "/api/v1/sessions/"+s.ID+"/producer/rotate", "", bearer(tok)); code != 401 && code != 403 {
			t.Fatalf("rotate with a non-host credential: %d", code)
		}
	}
}

func TestRoomProducerLinkAndLobby(t *testing.T) {
	h := newRoomsHarness(t)
	room := h.createRoom("The Podcast")
	if room.Producer.Enabled {
		t.Fatal("a new room has no producer link until the host issues one")
	}
	code, raw := h.do("POST", "/api/v1/rooms/"+room.ID+"/producer/rotate", "", nil)
	if code != 200 {
		t.Fatalf("room producer rotate: %d %s", code, raw)
	}
	var v RoomView
	_ = json.Unmarshal(raw, &v)
	u, _ := url.Parse(v.Producer.URL)
	if !v.Producer.Enabled || u.Query().Get("room") != room.Slug {
		t.Fatalf("room producer link: %+v", v.Producer)
	}
	tok := u.Query().Get("token")
	if tok == secretOf(t, room) {
		t.Fatal("producer link equals the guest link")
	}
	// The lobby accepts it and says producer; the guest link stays a guest.
	if code, l := h.lobby(room.Slug, tok); code != 200 || l.Role != signaling.RoleProducer || l.Live {
		t.Fatalf("producer lobby: %d %+v", code, l)
	}
	if _, l := h.lobby(room.Slug, secretOf(t, room)); l.Role != "" {
		t.Fatalf("guest lobby role: %+v", l)
	}
	m := h.start(room.ID)
	// The meeting's producer endpoint shows the room's link.
	if sv := h.producerLink(m.SessionID, m.HostToken, ""); sv.URL != v.Producer.URL || sv.RoomID != room.ID {
		t.Fatalf("room meeting producer link: %+v", sv)
	}
	host, _ := h.dial(m.SessionID, m.HostToken, nil)
	hello(t, host, "Ben", "host")
	admission(t, host)
	p, code := h.dial(m.SessionID, tok, nil)
	if code != 101 {
		t.Fatalf("room producer link refused: %d", code)
	}
	hello(t, p, "Pat", "guest")
	if adm := admission(t, p); adm.Status != "pending" {
		t.Fatalf("room producer: %+v", adm)
	}
	// Room auto-admit lets guests in but never a producer.
	code, _ = h.do("PATCH", "/api/v1/rooms/"+room.ID, `{"settings":{"autoAdmit":true}}`, nil)
	if code != 200 {
		t.Fatal(code)
	}
	if code, _ := h.do("POST", "/api/v1/rooms/"+room.ID+"/producer/revoke", "", nil); code != 200 {
		t.Fatalf("room producer revoke: %d", code)
	}
	if adm := admission(t, p); adm.Status != "denied" {
		t.Fatalf("waiting producer after revoke: %+v", adm)
	}
	if code, _ := h.lobby(room.Slug, tok); code != 401 {
		t.Fatalf("revoked room producer lobby: %d", code)
	}
}

func TestProducerNeverAutoAdmitted(t *testing.T) {
	h := newRoomsHarness(t)
	code, raw := h.do("POST", "/api/v1/sessions", `{"title":"Open","autoAdmit":true}`, nil)
	if code != 200 {
		t.Fatal(code)
	}
	var s CreateSessionResponse
	_ = json.Unmarshal(raw, &s)
	tok := linkToken(t, h.producerLink(s.ID, s.HostToken, "rotate").URL)
	g, _ := h.dial(s.ID, s.Token, nil)
	hello(t, g, "Guest", "guest")
	if adm := admission(t, g); adm.Status != "admitted" {
		t.Fatalf("auto-admit guest: %+v", adm)
	}
	p, _ := h.dial(s.ID, tok, nil)
	hello(t, p, "Pat", "producer")
	if adm := admission(t, p); adm.Status != "pending" {
		t.Fatalf("producer auto-admitted: %+v", adm)
	}
}

// TestProducerPermissionMatrix walks every host-only and host-assist
// signaling message as producer and as guest. Matrix (✓ allowed, ✗ action_error forbidden):
//
//	message              host  producer               guest
//	admit / deny         ✓     ✓ waiting guests only  ✗
//	record_start/stop    ✓     ✓ (never recorded)     ✗
//	preflight refresh    ✓     ✓                      ignored
//	mute_request         ✓     ✓ not the host         ✗
//	remove               ✓     ✓ guests only          ✗
//	room_lock            ✓     ✗                      ✗
//	screen_stop          ✓     ✗                      ✗
//	caption_control      ✓     ✗                      ✗
//	chirp                ✓     ✗                      ✗
//	hand (lower other)   ✓     ✗                      ✗
//	dashboard, guest join_request  host + producer;  producer join_request host only
func TestProducerPermissionMatrix(t *testing.T) {
	h := newRoomsHarness(t)
	h.srv.Upload = &upload.Handler{Store: h.st, Config: h.cfg}
	s := h.instantSession()
	ptok := linkToken(t, h.producerLink(s.ID, s.HostToken, "rotate").URL)

	host, _ := h.dial(s.ID, s.HostToken, nil)
	hello(t, host, "Host", "host")
	admission(t, host)

	prod, _ := h.dial(s.ID, ptok, nil)
	hello(t, prod, "Pat", "guest")
	prodAdm := admission(t, prod)
	pid := joinRequestFor(t, host, signaling.RoleProducer)
	if pid != prodAdm.ParticipantID {
		t.Fatal("join request names another participant")
	}
	sendEnv(t, host, signaling.TypeAdmit, signaling.AdmitPayload{ParticipantID: pid})
	prodAdm = admission(t, prod)
	if prodAdm.Status != "admitted" || prodAdm.Role != signaling.RoleProducer || prodAdm.UploadToken == "" {
		t.Fatalf("producer admission: %+v", prodAdm)
	}

	// The producer sees guest join requests and admits one.
	ga, _ := h.dial(s.ID, s.Token, nil)
	hello(t, ga, "Ada", "guest")
	gaID := joinRequestFor(t, prod, signaling.RoleGuest)
	sendEnv(t, prod, signaling.TypeAdmit, signaling.AdmitPayload{ParticipantID: gaID})
	until(t, ga, "admitted", func(env signaling.Envelope) bool {
		var a signaling.AdmissionPayload
		_ = json.Unmarshal(env.Payload, &a)
		return env.Type == signaling.TypeAdmission && a.Status == "admitted"
	})

	// A second producer: only the host decides; producer 2's request doesn't reach producer 1.
	prod2, _ := h.dial(s.ID, ptok, nil)
	hello(t, prod2, "Quinn", "guest")
	p2 := joinRequestFor(t, host, signaling.RoleProducer)
	sendEnv(t, prod, signaling.TypeAdmit, signaling.AdmitPayload{ParticipantID: p2})
	forbidden(t, prod, signaling.TypeAdmit)
	sendEnv(t, prod, signaling.TypeDeny, signaling.DenyPayload{ParticipantID: p2})
	forbidden(t, prod, signaling.TypeDeny)
	sendEnv(t, host, signaling.TypeDeny, signaling.DenyPayload{ParticipantID: p2})
	if adm := admissionWith(t, prod2, "denied"); adm.Status != "denied" {
		t.Fatalf("host deny producer 2: %+v", adm)
	}
	// A producer can't "deny" an admitted guest (that is removal).
	sendEnv(t, prod, signaling.TypeDeny, signaling.DenyPayload{ParticipantID: gaID})
	forbidden(t, prod, signaling.TypeDeny)

	// The producer declines a waiting guest; a guest can't admit anyone.
	gb, _ := h.dial(s.ID, s.Token, nil)
	hello(t, gb, "Bo", "guest")
	gbID := joinRequestFor(t, prod, signaling.RoleGuest)
	sendEnv(t, ga, signaling.TypeAdmit, signaling.AdmitPayload{ParticipantID: gbID})
	forbidden(t, ga, signaling.TypeAdmit)
	sendEnv(t, prod, signaling.TypeDeny, signaling.DenyPayload{ParticipantID: gbID})
	if adm := admissionWith(t, gb, "denied"); adm.Status != "denied" || adm.Reason != signaling.ReasonDeclined {
		t.Fatalf("producer decline: %+v", adm)
	}
	gc, _ := h.dial(s.ID, s.Token, nil)
	hello(t, gc, "Cy", "guest")
	gcID := joinRequestFor(t, prod, signaling.RoleGuest)
	sendEnv(t, prod, signaling.TypeAdmit, signaling.AdmitPayload{ParticipantID: gcID})
	if adm := admissionWith(t, gc, "admitted"); adm.Status != "admitted" {
		t.Fatalf("producer admit 2: %+v", adm)
	}

	// Dashboard: host and producer, never a guest.
	next(t, prod, signaling.TypeDashboard)
	// (a guest never getting it is checked last: a timed-out read ends a socket)

	// Recording: a guest can't start; a producer can, and is never recorded.
	sendEnv(t, ga, signaling.TypeRecordStart, nil)
	forbidden(t, ga, signaling.TypeRecordStart)
	sendEnv(t, prod, signaling.TypeRecordStart, nil)
	seen := until(t, prod, "recording room_state", func(env signaling.Envelope) bool {
		var st signaling.RoomStatePayload
		_ = json.Unmarshal(env.Payload, &st)
		return env.Type == signaling.TypeRoomState && st.Recording
	})
	for _, typ := range seen {
		if typ == signaling.TypeRecordStart {
			t.Fatal("a producer must never get record_start")
		}
	}
	var start signaling.RecordStartPayload
	_ = json.Unmarshal(next(t, gc, signaling.TypeRecordStart), &start)
	missing, err := h.st.ListMissingTakeParticipants(start.TakeID)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{}
	for _, m := range missing {
		expected[m.ID] = true
	}
	if expected[prodAdm.ParticipantID] || !expected[gaID] || !expected[gcID] || len(expected) != 3 {
		t.Fatalf("take expects %v; want host + 2 guests, never the producer", expected)
	}
	// The producer's grant can't register a track; a guest's can.
	register := func(tok string) int {
		code, _ := h.do("POST", "/api/v1/takes/"+start.TakeID+"/tracks", `{"kind":"video","mime":"video/webm","label":"Camera"}`, bearer(tok))
		return code
	}
	if code := register(prodAdm.UploadToken); code != http.StatusForbidden {
		t.Fatalf("producer track registration: %d, want 403", code)
	}
	gcGrant := signaling.MintUploadToken(gcID, s.ID, time.Hour, h.cfg.SecretKey)
	if code := register(gcGrant); code != http.StatusCreated {
		t.Fatalf("guest track registration: %d", code)
	}
	sendEnv(t, ga, signaling.TypeRecordStop, nil)
	forbidden(t, ga, signaling.TypeRecordStop)
	sendEnv(t, prod, signaling.TypeRecordStop, nil)
	next(t, gc, signaling.TypeRecordStop)

	// Pre-flight refresh before a take: the producer may ask.
	sendEnv(t, prod, signaling.TypePreflight, signaling.PreflightPayload{Refresh: true, Items: []signaling.PreflightItem{}})
	until(t, gc, "preflight refresh", func(env signaling.Envelope) bool {
		var p signaling.PreflightPayload
		_ = json.Unmarshal(env.Payload, &p)
		return env.Type == signaling.TypePreflight && p.Refresh
	})

	// Mute: guests and producers, never the host; guests can't.
	sendEnv(t, prod, signaling.TypeMuteRequest, signaling.MuteRequestPayload{ParticipantID: gaID})
	next(t, ga, signaling.TypeMuteRequest)
	hostID := ""
	for _, m := range missing {
		if m.Role == signaling.RoleHost {
			hostID = m.ID
		}
	}
	sendEnv(t, prod, signaling.TypeMuteRequest, signaling.MuteRequestPayload{ParticipantID: hostID})
	forbidden(t, prod, signaling.TypeMuteRequest)
	sendEnv(t, ga, signaling.TypeMuteRequest, signaling.MuteRequestPayload{ParticipantID: gcID})
	forbidden(t, ga, signaling.TypeMuteRequest)

	// Host-only controls stay host-only.
	for _, tc := range []struct {
		typ     string
		payload any
	}{
		{signaling.TypeRoomLock, signaling.RoomLockPayload{Locked: true}},
		{signaling.TypeScreenStop, signaling.ScreenStopPayload{ParticipantID: gaID}},
		{signaling.TypeCaptionControl, signaling.CaptionControlPayload{Enabled: true}},
		{signaling.TypeChirp, signaling.ChirpPayload{}},
		{signaling.TypeHand, signaling.HandPayload{ParticipantID: gcID, Raised: false}},
	} {
		for name, c := range map[string]*websocket.Conn{"producer": prod, "guest": ga} {
			sendEnv(t, c, tc.typ, tc.payload)
			t.Logf("%s %s", name, tc.typ)
			forbidden(t, c, tc.typ)
		}
	}
	if h.srv.Hub.IsRoomLocked(s.ID) {
		t.Fatal("room locked by a non-host")
	}

	// Captions never transcribe a producer.
	if _, ok := h.srv.Hub.AdmittedParticipant(s.ID, prodAdm.ParticipantID); ok {
		t.Fatal("producer audio would be captioned")
	}
	if _, ok := h.srv.Hub.AdmittedParticipant(s.ID, gaID); !ok {
		t.Fatal("guest audio must still be captioned")
	}

	// Remove: guests only, never the host.
	sendEnv(t, prod, signaling.TypeRemove, signaling.RemovePayload{ParticipantID: hostID})
	forbidden(t, prod, signaling.TypeRemove)
	sendEnv(t, ga, signaling.TypeRemove, signaling.RemovePayload{ParticipantID: gcID})
	forbidden(t, ga, signaling.TypeRemove)
	sendEnv(t, prod, signaling.TypeRemove, signaling.RemovePayload{ParticipantID: gcID})
	if adm := admission(t, gc); adm.Status != "denied" || adm.Reason != signaling.ReasonRemoved {
		t.Fatalf("producer remove guest: %+v", adm)
	}

	// The producer can't end the meeting, change settings or touch any host route.
	assertHostRoutesRefuse(t, h, s.ID, start.TakeID, map[string]string{"producer link": ptok, "producer grant": prodAdm.UploadToken})
	if sess, _ := h.st.GetSession(s.ID); sess.EndedAt != nil {
		t.Fatal("meeting ended by a producer")
	}
	// A guest never gets the dashboard (last: the timed-out read ends ga's socket).
	next(t, prod, signaling.TypeDashboard)
	silentFor(t, ga, signaling.TypeDashboard, 1500*time.Millisecond)
}

// assertHostRoutesRefuse sends every host-management HTTP route with each
// credential from the local machine (management position) and expects 401/403.
func assertHostRoutesRefuse(t *testing.T, h *roomsHarness, sessionID, takeID string, creds map[string]string) {
	t.Helper()
	room := h.createRoom("Assist check")
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/sessions", `{}`},
		{"PATCH", "/api/v1/sessions/" + sessionID + "/settings", `{"recording_mic_mode":"raw"}`},
		{"POST", "/api/v1/sessions/" + sessionID + "/invite", ""},
		{"POST", "/api/v1/sessions/" + sessionID + "/end", ""},
		{"GET", "/api/v1/sessions/" + sessionID + "/producer", ""},
		{"POST", "/api/v1/sessions/" + sessionID + "/producer/rotate", ""},
		{"POST", "/api/v1/sessions/" + sessionID + "/producer/revoke", ""},
		{"GET", "/api/v1/sessions/" + sessionID + "/livestream", ""},
		{"POST", "/api/v1/sessions/" + sessionID + "/livestream", `{"destinations":[]}`},
		{"DELETE", "/api/v1/sessions/" + sessionID + "/livestream", ""},
		{"GET", "/api/v1/sessions/" + sessionID + "/captions/ingest", ""},
		{"GET", "/api/v1/sessions/" + sessionID + "/captions/transcript?format=vtt", ""},
		{"GET", "/api/v1/home", ""},
		{"GET", "/api/v1/doctor", ""},
		{"GET", "/api/v1/rooms", ""},
		{"POST", "/api/v1/rooms", `{"name":"x"}`},
		{"PATCH", "/api/v1/rooms/" + room.ID, `{"name":"y"}`},
		{"DELETE", "/api/v1/rooms/" + room.ID, ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/rotate", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/revoke", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/start", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/producer/rotate", ""},
		{"POST", "/api/v1/rooms/" + room.ID + "/producer/revoke", ""},
		{"GET", "/api/v1/takes/" + takeID, ""},
		{"POST", "/api/v1/takes/" + takeID + "/postprocess", ""},
		{"GET", "/api/v1/takes/" + takeID + "/download", ""},
		{"GET", "/api/v1/takes/" + takeID + "/originals", ""},
		{"GET", "/api/v1/takes/" + takeID + "/integrations", ""},
		{"POST", "/api/v1/takes/" + takeID + "/transcript", ""},
	}
	for name, tok := range creds {
		for _, tc := range routes {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:7443"+tc.path, bytes.NewBufferString(tc.body))
			r.RemoteAddr = "127.0.0.1:40000"
			r.Header.Set("Authorization", "Bearer "+tok)
			w := httptest.NewRecorder()
			h.srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden && w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s = %d, want 401/403", tc.method, tc.path, name, w.Code)
			}
		}
	}
	// The host WebSocket needs a host token; a producer link never yields host.
	for name, tok := range creds {
		c, code := h.dial(sessionID, tok, nil)
		if code != 101 {
			continue // a grant is not a join credential: refused outright
		}
		hello(t, c, "X", "host")
		if adm := admission(t, c); adm.Role == signaling.RoleHost {
			t.Fatalf("%s joined as host", name)
		}
	}
}

func TestRecordingMicDefaultSettingBroadcast(t *testing.T) {
	h := newRoomsHarness(t)
	s := h.instantSession()
	g, _ := h.dial(s.ID, s.Token, nil)
	hello(t, g, "Guest", "guest")
	var st signaling.RoomStatePayload
	_ = json.Unmarshal(next(t, g, signaling.TypeRoomState), &st)
	if st.RecordingMicMode != signaling.RecordingMicAuto {
		t.Fatalf("default recording mic: %+v", st)
	}
	if code, _ := h.do("PATCH", "/api/v1/sessions/"+s.ID+"/settings", `{"recording_mic_mode":"loud"}`, bearer(s.HostToken)); code != 400 {
		t.Fatalf("invalid mode accepted: %d", code)
	}
	code, raw := h.do("PATCH", "/api/v1/sessions/"+s.ID+"/settings", `{"recording_mic_mode":"raw"}`, bearer(s.HostToken))
	if code != 200 || !strings.Contains(string(raw), `"recording_mic_mode":"raw"`) {
		t.Fatalf("set raw: %d %s", code, raw)
	}
	// Waiting guests learn it too (they open their recording mic before admission).
	_ = json.Unmarshal(next(t, g, signaling.TypeRoomState), &st)
	if st.RecordingMicMode != signaling.RecordingMicRaw {
		t.Fatalf("broadcast recording mic: %+v", st)
	}
	// Session detail carries it for the green room.
	if _, raw := h.do("GET", "/api/v1/sessions/"+s.ID, "", bearer(s.Token)); !strings.Contains(string(raw), `"recording_mic_mode":"raw"`) {
		t.Fatalf("detail: %s", raw)
	}
	// Rooms copy it onto new meetings, validated.
	room := h.createRoom("Studio")
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+room.ID, `{"settings":{"recording":{"recording_mic_mode":"x"}}}`, nil); code != 400 {
		t.Fatalf("invalid room mode: %d", code)
	}
	if code, _ := h.do("PATCH", "/api/v1/rooms/"+room.ID, `{"settings":{"recording":{"recording_mic_mode":"clean"}}}`, nil); code != 200 {
		t.Fatalf("room mode: %d", code)
	}
	m := h.start(room.ID)
	if _, raw := h.do("GET", "/api/v1/sessions/"+m.SessionID+"/settings", "", bearer(m.HostToken)); !strings.Contains(string(raw), `"recording_mic_mode":"clean"`) {
		t.Fatalf("room meeting settings: %s", raw)
	}
}
