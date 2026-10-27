package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/buildinfo"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

const testDesktopToken = "desktop-test-token-0123456789abcdef0123456789abcdef"

func desktopTestServer(t *testing.T) (*Server, *httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{DataDir: t.TempDir(), PublicURL: "https://studio.tail1234.ts.net", SecretKey: []byte("desktop-test-host-secret-0123456789")}
	hub := signaling.NewHub(st, cfg, nil)
	t.Cleanup(hub.Close)
	srv := &Server{Config: cfg, Store: st, Hub: hub, DesktopToken: testDesktopToken}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, st
}

func getDesktopState(t *testing.T, base string) DesktopState {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/v1/desktop/state", nil)
	req.Header.Set("Authorization", "Bearer "+testDesktopToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("desktop state: %v %v", err, resp)
	}
	defer resp.Body.Close()
	var st DesktopState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

// The Mac app decodes macapp/Tests/StudioLinkCoreTests/Fixtures/desktop-state.json;
// it must stay a valid DesktopState so the two sides cannot drift silently.
func TestDesktopStateFixtureMatchesContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "macapp", "Tests", "StudioLinkCoreTests", "Fixtures", "desktop-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var st DesktopState
	if err := dec.Decode(&st); err != nil {
		t.Fatalf("fixture does not match DesktopState: %v", err)
	}
	again, _ := json.Marshal(st)
	var a, b map[string]any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(again, &b)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("fixture fields are not all produced by DesktopState:\nfixture %v\nencoded %v", a, b)
	}
}

func TestDesktopStateRequiresLoopbackAndToken(t *testing.T) {
	srv, ts, _ := desktopTestServer(t)
	cases := []struct {
		name   string
		mutate func(r *http.Request)
		want   int
	}{
		{"no token", func(r *http.Request) {}, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		{"host-ish query token ignored", func(r *http.Request) { r.URL.RawQuery = "token=" + testDesktopToken }, http.StatusUnauthorized},
		{"funnel", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testDesktopToken)
			r.Header.Set("Tailscale-Funnel-Request", "?1")
		}, http.StatusForbidden},
		{"serve identity", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testDesktopToken)
			r.Header.Set("Tailscale-User-Login", "owner@example.com")
		}, http.StatusForbidden},
		{"forwarded", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testDesktopToken)
			r.Header.Set("X-Forwarded-For", "100.64.0.2")
		}, http.StatusForbidden},
		{"tailnet host", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testDesktopToken)
			r.Host = "studio.tail1234.ts.net"
		}, http.StatusForbidden},
		{"remote address", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testDesktopToken)
			r.RemoteAddr = "100.64.0.2:5555"
		}, http.StatusForbidden},
		{"browser origin", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+testDesktopToken)
			r.Header.Set("Origin", "http://127.0.0.1:7443")
		}, http.StatusForbidden},
		{"ok", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testDesktopToken) }, http.StatusOK},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/desktop/state", nil)
		r.RemoteAddr = "127.0.0.1:40000"
		tc.mutate(r)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: got %d want %d", tc.name, w.Code, tc.want)
		}
		if strings.Contains(w.Body.String(), testDesktopToken) {
			t.Errorf("%s: response echoes the desktop token", tc.name)
		}
	}
	// An unconfigured token disables the endpoint entirely.
	disabled := &Server{Config: srv.Config, Store: srv.Store, Hub: srv.Hub}
	r := httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/desktop/state", nil)
	r.RemoteAddr = "127.0.0.1:40000"
	r.Header.Set("Authorization", "Bearer ")
	w := httptest.NewRecorder()
	disabled.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty desktop token served state: %d", w.Code)
	}
	// Over a real socket too (no forwarded headers, loopback peer).
	_ = getDesktopState(t, ts.URL)
}

func TestDesktopStateReportsWaitingRecordingAndNoSecrets(t *testing.T) {
	_, ts, _ := desktopTestServer(t)
	idle := getDesktopState(t, ts.URL)
	if idle.Product != buildinfo.Product || idle.BuildID != buildinfo.ID || idle.PID == 0 || idle.Recording || len(idle.Sessions) != 0 {
		t.Fatalf("idle state: %+v", idle)
	}

	resp, err := adminPost(ts.URL+"/api/v1/sessions", `{"title":"Desk","autoAdmit":false}`)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create: %v %v", err, resp)
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

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
	dial := func(token, role, name string) *websocket.Conn {
		c, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s", wsURL, sess.ID, token), nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(signaling.HelloPayload{Name: name, Role: role})
		_ = c.WriteJSON(signaling.Envelope{Type: signaling.TypeHello, Payload: raw})
		return c
	}
	host := dial(sess.HostToken, "host", "Host")
	defer host.Close()
	guest := dial(inv["token"], "guest", "Ada")
	defer guest.Close()

	waitFor := func(desc string, ok func(DesktopState) bool) DesktopState {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			st := getDesktopState(t, ts.URL)
			if ok(st) {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", desc, st)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	st := waitFor("waiting guest", func(s DesktopState) bool { return s.WaitingCount == 1 })
	if len(st.Sessions) != 1 || st.Sessions[0].Title != "Desk" || !st.Sessions[0].HostConnected || st.Sessions[0].Waiting[0].Name != "Ada" {
		t.Fatalf("session summary: %+v", st.Sessions)
	}
	raw, _ := json.Marshal(st)
	for _, secret := range []string{sess.HostToken, inv["token"], testDesktopToken} {
		if secret == "" || bytes.Contains(raw, []byte(secret)) {
			t.Fatal("desktop state leaks a credential")
		}
	}

	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeRecordStart, Payload: json.RawMessage(`{}`)})
	st = waitFor("recording", func(s DesktopState) bool { return s.Recording })
	if st.Sessions[0].TakeNumber != 1 || len(st.Takes) != 1 || st.Takes[0].Status != "recording" || st.Takes[0].UploadsComplete {
		t.Fatalf("recording state: %+v", st)
	}
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeRecordStop, Payload: json.RawMessage(`{}`)})
	st = waitFor("stopped", func(s DesktopState) bool { return !s.Recording })
	if st.Takes[0].Status != "stopped" || st.Takes[0].UploadsComplete || st.Takes[0].MissingParticipants != 1 {
		t.Fatalf("stopped take with the host's media missing must not read as uploaded: %+v", st.Takes[0])
	}
}

// A take still recording after every socket left (the host's studio window
// closed mid-take) stays visible to the app [SL-P2-3].
func TestDesktopStateKeepsRecordingTakeWithoutSockets(t *testing.T) {
	_, ts, _ := desktopTestServer(t)
	resp, err := adminPost(ts.URL+"/api/v1/sessions", `{"title":"Solo","autoAdmit":false}`)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create: %v %v", err, resp)
	}
	var sess CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/ws"
	host, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s?session=%s&token=%s", wsURL, sess.ID, sess.HostToken), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(signaling.HelloPayload{Name: "Host", Role: "host"})
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeHello, Payload: raw})
	wait := func(desc string, ok func(DesktopState) bool) DesktopState {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			st := getDesktopState(t, ts.URL)
			if ok(st) {
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", desc, st)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	wait("host connected", func(s DesktopState) bool { return len(s.Sessions) == 1 && s.Sessions[0].HostConnected })
	_ = host.WriteJSON(signaling.Envelope{Type: signaling.TypeRecordStart, Payload: json.RawMessage(`{}`)})
	wait("recording", func(s DesktopState) bool { return s.Recording })
	_ = host.Close()
	st := wait("host gone, take still recording", func(s DesktopState) bool {
		return len(s.Sessions) == 1 && s.Sessions[0].Connected == 0 && !s.Sessions[0].HostConnected
	})
	if !st.Recording || !st.Sessions[0].Recording || st.Sessions[0].TakeNumber != 1 || st.Sessions[0].Waiting == nil {
		t.Fatalf("a recording take without sockets must still be reported: %+v", st)
	}
}
