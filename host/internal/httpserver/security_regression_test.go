package httpserver

// Regression tests for the SL-SEC-1 review findings (docs/SECURITY.md).

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

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
)

const testLaunchToken = "launch-test-token-0123456789abcdef0123456789abcdef"

func newSecHarness(t *testing.T) *roomsHarness {
	t.Helper()
	h := newRoomsHarness(t)
	h.srv.LaunchToken = testLaunchToken
	return h
}

// rawDo sends a request with exactly the given headers (no default admin
// credential), never following redirects.
func (h *roomsHarness) rawDo(method, path string, hdr http.Header) *http.Response {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+path, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// TestM4ManagementNeedsAdminCredential: a local process with only loopback
// access (no launch cookie, no desktop token) gets no host tokens or room
// secrets; the launch flow and the desktop token both work.
func TestM4ManagementNeedsAdminCredential(t *testing.T) {
	h := newSecHarness(t)
	routes := []struct{ method, path string }{
		{"GET", "/api/v1/home"}, {"GET", "/api/v1/rooms"}, {"POST", "/api/v1/sessions"},
		{"GET", "/api/v1/doctor"}, {"GET", "/api/v1/tailscale/status"},
	}
	for _, rt := range routes {
		if resp := h.rawDo(rt.method, rt.path, nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s without credential: %d, want 403", rt.method, rt.path, resp.StatusCode)
		}
		if resp := h.rawDo(rt.method, rt.path, http.Header{"Authorization": {"Bearer wrong-" + testDesktopToken}}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with wrong token: %d", rt.method, rt.path, resp.StatusCode)
		}
	}
	if resp := h.rawDo("GET", "/api/v1/home", http.Header{"Authorization": {"Bearer " + testDesktopToken}}); resp.StatusCode != 200 {
		t.Fatalf("desktop token: %d", resp.StatusCode)
	}

	// A wrong or stale launch token: redirected without the token, no cookie.
	resp := h.rawDo("GET", "/?launch=stale&room=r1", nil)
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) != 0 {
		t.Fatalf("stale launch: %d cookies=%v", resp.StatusCode, resp.Cookies())
	}
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "launch") || loc != "/?room=r1" {
		t.Fatalf("stale launch kept the token or the query: %q", loc)
	}
	// A non-navigation fetch cannot mint the cookie either.
	resp = h.rawDo("GET", "/?launch="+testLaunchToken, http.Header{"Sec-Fetch-Mode": {"cors"}})
	if len(resp.Cookies()) != 0 {
		t.Fatal("cookie issued to a non-navigation request")
	}
	// The real launch: cookie set, token stripped from the URL.
	resp = h.rawDo("GET", "/?launch="+testLaunchToken+"&room=r1", http.Header{"Sec-Fetch-Mode": {"navigate"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/?room=r1" {
		t.Fatalf("launch: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatalf("admin cookie attributes: %+v", cookies)
	}
	if strings.Contains(cookies[0].Value, testLaunchToken) {
		t.Fatal("cookie carries the launch token")
	}
	cookie := cookies[0].Name + "=" + cookies[0].Value
	if resp := h.rawDo("GET", "/api/v1/home", http.Header{"Cookie": {cookie}}); resp.StatusCode != 200 {
		t.Fatalf("home with admin cookie: %d", resp.StatusCode)
	}
	// DNS rebinding: the browser would send the cookie only to its own
	// origin, but even with it a foreign Host header is not local management.
	req, _ := http.NewRequest("GET", h.ts.URL+"/api/v1/home", nil)
	req.Host = "rebind.attacker.example"
	req.Header.Set("Cookie", cookie)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding Host with cookie: %v %v", err, r)
	}
	// A forged cookie value is refused.
	if resp := h.rawDo("GET", "/api/v1/home", http.Header{"Cookie": {cookies[0].Name + "=forged"}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("forged cookie: %d", resp.StatusCode)
	}
	// The launch token itself is not a credential for the API.
	if resp := h.rawDo("GET", "/api/v1/home", http.Header{"Authorization": {"Bearer " + testLaunchToken}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("launch token as bearer: %d", resp.StatusCode)
	}
	// Through Funnel the launch is never honoured.
	resp = h.rawDo("GET", "/?launch="+testLaunchToken, http.Header{"Tailscale-Funnel-Request": {"?1"}})
	if len(resp.Cookies()) != 0 {
		t.Fatal("cookie issued through Funnel")
	}
}

// TestM3TailscaleStatusHostOnlyAndCached: refused to guests and Funnel, and
// one CLI fork serves a burst of requests.
func TestM3TailscaleStatusHostOnlyAndCached(t *testing.T) {
	h := newSecHarness(t)
	if resp := h.rawDo("GET", "/api/v1/tailscale/status", http.Header{"Tailscale-Funnel-Request": {"?1"}, "Authorization": {"Bearer " + testDesktopToken}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("funnel: %d", resp.StatusCode)
	}
	var c tsStatusCache
	now := time.Unix(1000, 0)
	calls := 0
	fetch := func() []byte { calls++; return []byte(fmt.Sprintf(`{"n":%d}`, calls)) }
	for i := 0; i < 50; i++ {
		c.get(now.Add(time.Duration(i)*50*time.Millisecond), fetch)
	}
	if calls != 1 {
		t.Fatalf("burst forked tailscale %d times", calls)
	}
	if got := string(c.get(now.Add(tsStatusTTL+time.Second), fetch)); got != `{"n":2}` || calls != 2 {
		t.Fatalf("after TTL: %s calls=%d", got, calls)
	}
}

// TestM2SessionDetailIsTrimmedPerCredential: an invite holder sees only the
// waiting-room view, an admitted participant also sees names, only the host
// sees takes, and nobody sees the invite hash.
func TestM2SessionDetailIsTrimmedPerCredential(t *testing.T) {
	h := newSecHarness(t)
	code, raw := h.do("POST", "/api/v1/sessions", `{"title":"Detail","autoAdmit":false}`, nil)
	if code != 200 {
		t.Fatalf("create: %d %s", code, raw)
	}
	var sess CreateSessionResponse
	_ = json.Unmarshal(raw, &sess)
	host, _ := h.dial(sess.ID, sess.HostToken, nil)
	hello(t, host, "Host", "host")
	admission(t, host)
	guest, _ := h.dial(sess.ID, sess.Token, nil)
	hello(t, guest, "Guest", "guest")
	pending := admission(t, guest)
	var req struct{ ParticipantID string }
	_ = json.Unmarshal(next(t, host, signaling.TypeJoinRequest), &req)

	detail := func(token string) (int, map[string]any) {
		code, raw := h.do("GET", "/api/v1/sessions/"+sess.ID, "", http.Header{"Authorization": {"Bearer " + token}})
		var v map[string]any
		_ = json.Unmarshal(raw, &v)
		if strings.Contains(string(raw), "inviteTokenHash") || strings.Contains(string(raw), hashToken(sess.Token)) {
			t.Fatalf("invite hash serialized: %s", raw)
		}
		return code, v
	}
	// Invite only (even never admitted, even via Funnel).
	code, v := detail(sess.Token)
	if code != 200 || v["participants"] != nil || v["takes"] != nil || v["settings"] == nil {
		t.Fatalf("invite view: %d %v", code, v)
	}
	if s := v["session"].(map[string]any); s["title"] != "Detail" || len(s) != 2 {
		t.Fatalf("invite session view: %v", s)
	}
	// A pending participant's grant is not a participant view.
	if code, _ := detail(signaling.MintUploadToken(pending.ParticipantID, sess.ID, time.Hour, h.cfg.SecretKey)); code != 401 {
		t.Fatalf("pending grant: %d", code)
	}
	raw, _ = json.Marshal(signaling.AdmitPayload{ParticipantID: req.ParticipantID})
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeAdmit, Payload: raw})
	adm := admission(t, guest)
	code, v = detail(adm.UploadToken)
	parts, _ := v["participants"].([]any)
	if code != 200 || v["takes"] != nil || len(parts) != 2 {
		t.Fatalf("participant view: %d %v", code, v)
	}
	for _, p := range parts {
		if m := p.(map[string]any); m["name"] == nil || m["status"] != nil || m["joinedAt"] != nil {
			t.Fatalf("participant roster entry not trimmed: %v", m)
		}
	}
	// A grant for another session is refused.
	if code, _ := detail(signaling.MintUploadToken(req.ParticipantID, "other", time.Hour, h.cfg.SecretKey)); code != 401 {
		t.Fatalf("cross-session grant: %d", code)
	}
	code, v = detail(sess.HostToken)
	if code != 200 || v["takes"] == nil || v["participants"] == nil {
		t.Fatalf("host view: %d %v", code, v)
	}
}

// TestM1JoinRateLimitsAndWaitingRoomCap: upgrades are rate-limited per client
// address, the waiting room is capped without creating rows, and a socket
// that never says hello is closed.
func TestM1JoinRateLimitsAndWaitingRoomCap(t *testing.T) {
	h := newSecHarness(t)
	_, raw := h.do("POST", "/api/v1/sessions", `{"title":"Flood","autoAdmit":false}`, nil)
	var sess CreateSessionResponse
	_ = json.Unmarshal(raw, &sess)
	host, _ := h.dial(sess.ID, sess.HostToken, nil)
	hello(t, host, "Host", "host")
	admission(t, host)

	// Waiting-room cap: MaxPendingPerSession requests wait, the next is
	// refused as busy and leaves no participant row. Each comes from its own
	// (forwarded) address so the per-address bucket is not what stops it.
	fwd := func(i int) http.Header {
		return http.Header{"X-Forwarded-For": {fmt.Sprintf("100.64.1.%d", i)}}
	}
	for i := 0; i < signaling.MaxPendingPerSession; i++ {
		g, code := h.dial(sess.ID, sess.Token, fwd(i))
		if code != 101 {
			t.Fatalf("guest %d: %d", i, code)
		}
		hello(t, g, fmt.Sprintf("G%d", i), "guest")
		if adm := admission(t, g); adm.Status != "pending" {
			t.Fatalf("guest %d: %+v", i, adm)
		}
	}
	before, _ := h.st.ListParticipants(sess.ID)
	extra, _ := h.dial(sess.ID, sess.Token, fwd(200))
	hello(t, extra, "Extra", "guest")
	if adm := admission(t, extra); adm.Status != "denied" || adm.Reason != signaling.ReasonBusy {
		t.Fatalf("over cap: %+v", adm)
	}
	after, _ := h.st.ListParticipants(sess.ID)
	if len(after) != len(before) {
		t.Fatalf("denied-by-limit attempt created a row: %d -> %d", len(before), len(after))
	}

	// Per-address bucket: a burst from one address is cut off with 429.
	limited := 0
	for i := 0; i < joinAddrBurst+5; i++ {
		c, code := h.dial(sess.ID, sess.Token, http.Header{"X-Forwarded-For": {"203.0.113.7"}})
		if code == http.StatusTooManyRequests {
			limited++
		} else if c != nil {
			c.Close()
		}
	}
	if limited == 0 {
		t.Fatal("no 429 after a burst from one address")
	}
	// Another address is unaffected.
	if c, code := h.dial(sess.ID, sess.Token, http.Header{"X-Forwarded-For": {"203.0.113.8"}}); code != 101 {
		t.Fatalf("other address limited: %d", code)
	} else {
		c.Close()
	}
	// Bad tokens are rejected before they count against anyone.
	if _, code := h.dial(sess.ID, "nope", http.Header{"X-Forwarded-For": {"203.0.113.7"}}); code != 401 {
		t.Fatalf("bad token: %d", code)
	}
}

func TestM1LimiterRefills(t *testing.T) {
	l := newLimiter(2, 1)
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	got := []bool{l.allow("a"), l.allow("a"), l.allow("a")}
	if !got[0] || !got[1] || got[2] {
		t.Fatalf("burst of 2 not enforced: %v", got)
	}
	now = now.Add(time.Second)
	if !l.allow("a") || l.allow("a") {
		t.Fatal("refill of 1/s not applied")
	}
	if !l.allow("b") {
		t.Fatal("keys are independent")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1"
	r.Header.Set("X-Forwarded-For", "198.51.100.4, 10.0.0.1")
	if got := clientAddr(r); got != "198.51.100.4" {
		t.Fatalf("serve-forwarded address: %s", got)
	}
	r.RemoteAddr = "192.0.2.5:1"
	if got := clientAddr(r); got != "192.0.2.5" {
		t.Fatalf("non-loopback peer must not trust X-Forwarded-For: %s", got)
	}
}

func TestM1SilentSocketIsClosed(t *testing.T) {
	old := signaling.HelloTimeout
	signaling.HelloTimeout = 200 * time.Millisecond
	t.Cleanup(func() { signaling.HelloTimeout = old })
	h := newSecHarness(t)
	_, raw := h.do("POST", "/api/v1/sessions", `{"title":"Quiet"}`, nil)
	var sess CreateSessionResponse
	_ = json.Unmarshal(raw, &sess)
	c, code := h.dial(sess.ID, sess.Token, nil)
	if code != 101 {
		t.Fatal(code)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
				t.Fatal("socket without hello was not closed")
			}
			break
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.srv.Hub.Counts()["hubClients"] != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("silent socket still registered: %v", h.srv.Hub.Counts())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestL4RotatingLinksDeniesWaitingGuests: people waiting under a room link
// or invite that is rotated/revoked are denied; admitted people stay.
func TestL4RotatingLinksDeniesWaitingGuests(t *testing.T) {
	h := newSecHarness(t)
	room := h.createRoom("Show")
	secret := secretOf(t, room)
	start := h.start(room.ID)
	host, _ := h.dial(start.SessionID, start.HostToken, nil)
	hello(t, host, "Host", "host")
	admission(t, host)

	waitAndAdmit := func(token, name string, admit bool) *websocket.Conn {
		g, code := h.dial(start.SessionID, token, nil)
		if code != 101 {
			t.Fatalf("%s dial: %d", name, code)
		}
		hello(t, g, name, "guest")
		if adm := admission(t, g); adm.Status != "pending" {
			t.Fatalf("%s: %+v", name, adm)
		}
		var req struct{ ParticipantID string }
		_ = json.Unmarshal(next(t, host, signaling.TypeJoinRequest), &req)
		if admit {
			raw, _ := json.Marshal(signaling.AdmitPayload{ParticipantID: req.ParticipantID})
			_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeAdmit, Payload: raw})
			if adm := admission(t, g); adm.Status != "admitted" {
				t.Fatalf("%s admit: %+v", name, adm)
			}
		}
		return g
	}
	admitted := waitAndAdmit(secret, "Admitted", true)
	waiting := waitAndAdmit(secret, "Waiting", false)

	if code, _ := h.do("POST", "/api/v1/rooms/"+room.ID+"/rotate", "", nil); code != 200 {
		t.Fatalf("rotate: %d", code)
	}
	if adm := admission(t, waiting); adm.Status != "denied" {
		t.Fatalf("waiting guest after rotate: %+v", adm)
	}
	// The admitted guest gets no denial.
	_ = admitted.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	for {
		var env signaling.Envelope
		if err := admitted.ReadJSON(&env); err != nil {
			break
		}
		if env.Type == signaling.TypeAdmission {
			t.Fatalf("admitted guest got an admission change: %s", env.Payload)
		}
	}

	// Revoke: same for someone waiting under the rotated link.
	code, raw := h.do("GET", "/api/v1/rooms/"+room.ID, "", nil)
	var current RoomView
	_ = json.Unmarshal(raw, &current)
	if code != 200 {
		t.Fatal(code)
	}
	w2 := waitAndAdmit(secretOf(t, current), "Waiting2", false)
	if code, _ := h.do("POST", "/api/v1/rooms/"+room.ID+"/revoke", "", nil); code != 200 {
		t.Fatal(code)
	}
	if adm := admission(t, w2); adm.Status != "denied" {
		t.Fatalf("waiting guest after revoke: %+v", adm)
	}
}

func TestL4ReissuingInviteDeniesWaitingGuests(t *testing.T) {
	h := newSecHarness(t)
	_, raw := h.do("POST", "/api/v1/sessions", `{"title":"Invite"}`, nil)
	var sess CreateSessionResponse
	_ = json.Unmarshal(raw, &sess)
	host, _ := h.dial(sess.ID, sess.HostToken, nil)
	hello(t, host, "Host", "host")
	admission(t, host)
	g, _ := h.dial(sess.ID, sess.Token, nil)
	hello(t, g, "Guest", "guest")
	if adm := admission(t, g); adm.Status != "pending" {
		t.Fatalf("%+v", adm)
	}
	if code, raw := h.do("POST", "/api/v1/sessions/"+sess.ID+"/invite", "", http.Header{"Authorization": {"Bearer " + sess.HostToken}}); code != 200 {
		t.Fatalf("reissue: %d %s", code, raw)
	}
	if adm := admission(t, g); adm.Status != "denied" {
		t.Fatalf("waiting under old invite: %+v", adm)
	}
}

// TestL3HostTokenSocketNeedsManagementPosition: a host token alone does not
// give host control from the tailnet (private Serve) or Funnel.
func TestL3HostTokenSocketNeedsManagementPosition(t *testing.T) {
	h := newSecHarness(t)
	_, raw := h.do("POST", "/api/v1/sessions", `{"title":"L3"}`, nil)
	var sess CreateSessionResponse
	_ = json.Unmarshal(raw, &sess)
	for name, hdr := range map[string]http.Header{
		"private serve": {"X-Forwarded-For": {"100.64.0.7"}},
		"funnel":        {"Tailscale-Funnel-Request": {"?1"}},
	} {
		if _, code := h.dial(sess.ID, sess.HostToken, hdr); code != http.StatusForbidden {
			t.Errorf("%s host socket: %d, want 403", name, code)
		}
	}
	if c, code := h.dial(sess.ID, sess.HostToken, nil); code != 101 {
		t.Fatalf("local host socket: %d", code)
	} else {
		c.Close()
	}
}

// TestCSPAndSecurityHeaders: every response carries the CSP and friends.
func TestCSPAndSecurityHeaders(t *testing.T) {
	h := newSecHarness(t)
	for _, path := range []string{"/", "/join?session=x&token=y", "/health", "/api/v1/home"} {
		resp := h.rawDo("GET", path, nil)
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "connect-src 'self' ws: wss:", "worker-src 'self' blob:"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s CSP %q lacks %q", path, csp, want)
			}
		}
		// script-src is exactly 'self' plus 'wasm-unsafe-eval' (WebAssembly
		// compilation for background effects, SL-P1-7): never 'unsafe-eval',
		// 'unsafe-inline' or another source.
		script := strings.Fields(strings.Split(strings.Split(csp, "script-src")[1], ";")[0])
		if strings.Join(script, " ") != "'self' 'wasm-unsafe-eval'" {
			t.Errorf("%s CSP script-src = %v", path, script)
		}
		if strings.Contains(strings.ReplaceAll(csp, "'wasm-unsafe-eval'", ""), "unsafe-eval") {
			t.Errorf("%s CSP allows eval: %s", path, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s headers: %v", path, resp.Header)
		}
	}
}

func TestLaunchTokenNeverLogged(t *testing.T) {
	buf := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	h := newSecHarness(t)
	for _, tok := range []string{testLaunchToken, testLaunchToken + "x"} {
		if resp := h.rawDo("GET", "/?"+url.Values{"launch": {tok}}.Encode(), nil); resp.StatusCode != http.StatusSeeOther {
			t.Fatal(resp.StatusCode)
		}
	}
	if strings.Contains(buf.String(), testLaunchToken) || strings.Contains(buf.String(), testDesktopToken) {
		t.Fatalf("launch or desktop token logged: %s", buf.String())
	}
}
