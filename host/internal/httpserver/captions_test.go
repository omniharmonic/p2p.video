package httpserver

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/captions"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

type captionRig struct {
	srv  *Server
	ts   *httptest.Server
	sess CreateSessionResponse
	ws   string
}

func newCaptionRig(t *testing.T) *captionRig {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{DataDir: t.TempDir(), PublicURL: "https://studio.tail1234.ts.net", SecretKey: []byte("caption-test-host-secret-0123456789")}
	hub := signaling.NewHub(st, cfg, nil)
	t.Cleanup(hub.Close)
	mgr := captions.NewManager(hub)
	t.Cleanup(mgr.Close)
	hub.Captions = mgr
	srv := &Server{Config: cfg, Store: st, Hub: hub, DesktopToken: testDesktopToken, Captions: mgr}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	resp, err := adminPost(ts.URL+"/api/v1/sessions", `{"title":"Captions","autoAdmit":false}`)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create: %v %v", err, resp)
	}
	var sess CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	return &captionRig{srv: srv, ts: ts, sess: sess, ws: "ws" + strings.TrimPrefix(ts.URL, "http")}
}

// sigClient is a signaling socket that records every envelope it receives.
type sigClient struct {
	t    *testing.T
	conn *websocket.Conn
	envs chan signaling.Envelope
}

func (r *captionRig) join(t *testing.T, token, name, role string) *sigClient {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("%s/api/v1/ws?session=%s&token=%s", r.ws, r.sess.ID, token), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c := &sigClient{t: t, conn: conn, envs: make(chan signaling.Envelope, 256)}
	go func() {
		for {
			var env signaling.Envelope
			if conn.ReadJSON(&env) != nil {
				close(c.envs)
				return
			}
			if env.Type != signaling.TypeDashboard {
				c.envs <- env
			}
		}
	}()
	raw, _ := json.Marshal(signaling.HelloPayload{Name: name, Role: role})
	_ = conn.WriteJSON(signaling.Envelope{Type: signaling.TypeHello, Payload: raw})
	return c
}

func (c *sigClient) send(typ string, payload any) {
	raw, _ := json.Marshal(payload)
	_ = c.conn.WriteJSON(signaling.Envelope{Type: typ, Payload: raw})
}

// next waits for an envelope of type typ matching ok.
func (c *sigClient) next(typ string, ok func(json.RawMessage) bool) json.RawMessage {
	c.t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case env, open := <-c.envs:
			if !open {
				c.t.Fatalf("socket closed waiting for %s", typ)
			}
			if env.Type == typ && (ok == nil || ok(env.Payload)) {
				return env.Payload
			}
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

// none asserts no envelope of type typ arrives within d.
func (c *sigClient) none(typ string, d time.Duration) {
	c.t.Helper()
	deadline := time.After(d)
	for {
		select {
		case env, open := <-c.envs:
			if !open {
				return
			}
			if env.Type == typ {
				c.t.Fatalf("unexpected %s: %s", typ, env.Payload)
			}
		case <-deadline:
			return
		}
	}
}

func capAdmission(raw json.RawMessage) signaling.AdmissionPayload {
	var a signaling.AdmissionPayload
	_ = json.Unmarshal(raw, &a)
	return a
}

func captionState(raw json.RawMessage) signaling.CaptionStatePayload {
	var s signaling.CaptionStatePayload
	_ = json.Unmarshal(raw, &s)
	return s
}

func dialProvider(t *testing.T, base string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testDesktopToken)
	conn, resp, err := websocket.DefaultDialer.Dial(base+"/api/v1/captions/provider", h)
	if err != nil {
		t.Fatalf("provider dial: %v %v", err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.WriteJSON(captions.ProviderMessage{T: captions.MsgHello, Protocol: 1, Name: "Fake", Engine: "test", Languages: []string{"en-US"}})
	var ready captions.ProviderMessage
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil || ready.T != captions.MsgReady {
		t.Fatalf("no ready: %+v %v", ready, err)
	}
	return conn
}

func tone(ms int) []byte {
	var b bytes.Buffer
	for i := 0; i < captions.SampleRate*ms/1000; i++ {
		_ = binary.Write(&b, binary.LittleEndian, int16(math.Sin(float64(i)*2*math.Pi*300/captions.SampleRate)*10000))
	}
	return b.Bytes()
}

// End to end in process: the host's ingest feed carries a guest's audio to
// the provider under an opaque stream ID; the provider's result becomes a
// caption for every admitted participant (never the waiting room, which only
// learns that captions are on), and the final lands in the host's transcript.
func TestCaptionsEndToEnd(t *testing.T) {
	r := newCaptionRig(t)
	host := r.join(t, r.sess.HostToken, "Host", "host")
	host.next(signaling.TypeAdmission, nil)
	if st := captionState(host.next(signaling.TypeCaptionState, nil)); st.Enabled || st.Provider || st.Language != "en-US" {
		t.Fatalf("initial state %+v", st)
	}
	guest := r.join(t, r.sess.Token, "Guest <b>", "guest")
	pending := capAdmission(guest.next(signaling.TypeAdmission, nil))
	host.send(signaling.TypeAdmit, signaling.AdmitPayload{ParticipantID: pending.ParticipantID})
	guest.next(signaling.TypeAdmission, func(raw json.RawMessage) bool { return capAdmission(raw).Status == "admitted" })
	waiting := r.join(t, r.sess.Token, "Waiting", "guest")
	waiting.next(signaling.TypeAdmission, nil)

	// A guest cannot turn captions on.
	guest.send(signaling.TypeCaptionControl, signaling.CaptionControlPayload{Enabled: true})
	guest.next(signaling.TypeActionError, nil)

	provider := dialProvider(t, r.ws)
	host.next(signaling.TypeCaptionState, func(raw json.RawMessage) bool { return captionState(raw).Provider })
	host.send(signaling.TypeCaptionControl, signaling.CaptionControlPayload{Enabled: true})
	for _, c := range []*sigClient{host, guest, waiting} {
		c.next(signaling.TypeCaptionState, func(raw json.RawMessage) bool { s := captionState(raw); return s.Enabled && s.Provider })
	}

	// The browser's ingest socket needs the host token.
	if _, resp, err := websocket.DefaultDialer.Dial(r.ws+"/api/v1/sessions/"+r.sess.ID+"/captions/ingest?token="+r.sess.Token, nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("guest token opened the ingest: %v", err)
	}
	ingest, _, err := websocket.DefaultDialer.Dial(r.ws+"/api/v1/sessions/"+r.sess.ID+"/captions/ingest?token="+r.sess.HostToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ingest.Close()
	if _, resp, err := websocket.DefaultDialer.Dial(r.ws+"/api/v1/sessions/"+r.sess.ID+"/captions/ingest?token="+r.sess.HostToken, nil); err == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("second ingest: %v", err)
	}
	_ = ingest.WriteMessage(websocket.BinaryMessage, captions.EncodeFrame(pending.ParticipantID, tone(100)))
	// Audio claimed for a waiting participant is never relayed.
	_ = ingest.WriteMessage(websocket.BinaryMessage, captions.EncodeFrame("p_not-admitted", tone(100)))

	_ = provider.SetReadDeadline(time.Now().Add(3 * time.Second))
	var start captions.ProviderMessage
	if err := provider.ReadJSON(&start); err != nil || start.T != captions.MsgStreamStart {
		t.Fatalf("stream start: %+v %v", start, err)
	}
	if strings.Contains(start.StreamID, pending.ParticipantID) {
		t.Fatal("provider learned the participant ID")
	}
	kind, frame, err := provider.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage {
		t.Fatalf("audio frame: %v", err)
	}
	key, pcm, err := captions.DecodeFrame(frame)
	if err != nil || key != start.StreamID || !bytes.Equal(pcm, tone(100)) {
		t.Fatalf("relayed frame key %q err %v", key, err)
	}
	_ = provider.WriteJSON(captions.ProviderMessage{T: captions.MsgResult, StreamID: start.StreamID, SegmentID: "1", Text: "hello from the guest", IsFinal: true, StartMs: 0, EndMs: 90})

	for _, c := range []*sigClient{host, guest} {
		var cap signaling.CaptionPayload
		_ = json.Unmarshal(c.next(signaling.TypeCaption, nil), &cap)
		if cap.Text != "hello from the guest" || cap.ParticipantID != pending.ParticipantID || cap.Name != "Guest <b>" || !cap.IsFinal {
			t.Fatalf("caption %+v", cap)
		}
	}
	waiting.none(signaling.TypeCaption, 300*time.Millisecond)

	// Transcript: host only, VTT or JSON.
	get := func(q, token string) (int, string) {
		resp, err := http.Get(r.ts.URL + "/api/v1/sessions/" + r.sess.ID + "/captions/transcript?" + q + "&token=" + token)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("format=vtt", r.sess.Token); code != http.StatusUnauthorized {
		t.Fatalf("guest transcript: %d", code)
	}
	code, vtt := get("format=vtt", r.sess.HostToken)
	if code != 200 || !strings.Contains(vtt, "<v Guest &lt;b&gt;>hello from the guest") {
		t.Fatalf("vtt %d %q", code, vtt)
	}
	code, js := get("format=json", r.sess.HostToken)
	var tr captions.Transcript
	if code != 200 || json.Unmarshal([]byte(js), &tr) != nil || len(tr.Segments) != 1 || tr.Source != "live-captions" {
		t.Fatalf("json %d %s", code, js)
	}

	// Host turns captions off: everyone (waiting room included) is told.
	host.send(signaling.TypeCaptionControl, signaling.CaptionControlPayload{Enabled: false})
	waiting.next(signaling.TypeCaptionState, func(raw json.RawMessage) bool { return !captionState(raw).Enabled })
	var end captions.ProviderMessage
	if err := provider.ReadJSON(&end); err != nil || end.T != captions.MsgStreamEnd || end.StreamID != start.StreamID {
		t.Fatalf("stream end: %+v %v", end, err)
	}
}

// The provider socket follows the desktop endpoint's rules: direct loopback,
// desktop token, no browser Origin, never through Serve/Funnel. One at a time,
// and it must open with a protocol 1 hello.
func TestCaptionProviderAuthorization(t *testing.T) {
	r := newCaptionRig(t)
	cases := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"no token", func(*http.Request) {}, http.StatusUnauthorized},
		{"host token", func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+r.sess.HostToken) }, http.StatusUnauthorized},
		{"funnel", func(q *http.Request) {
			q.Header.Set("Authorization", "Bearer "+testDesktopToken)
			q.Header.Set("Tailscale-Funnel-Request", "?1")
		}, http.StatusForbidden},
		{"serve", func(q *http.Request) {
			q.Header.Set("Authorization", "Bearer "+testDesktopToken)
			q.Header.Set("Tailscale-User-Login", "owner@example.com")
		}, http.StatusForbidden},
		{"remote", func(q *http.Request) {
			q.Header.Set("Authorization", "Bearer "+testDesktopToken)
			q.RemoteAddr = "100.64.0.2:5555"
		}, http.StatusForbidden},
		{"tailnet host", func(q *http.Request) {
			q.Header.Set("Authorization", "Bearer "+testDesktopToken)
			q.Host = "studio.tail1234.ts.net"
		}, http.StatusForbidden},
		{"browser origin", func(q *http.Request) {
			q.Header.Set("Authorization", "Bearer "+testDesktopToken)
			q.Header.Set("Origin", "http://127.0.0.1:7443")
		}, http.StatusForbidden},
	}
	for _, tc := range cases {
		q := httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/captions/provider", nil)
		q.RemoteAddr = "127.0.0.1:40000"
		tc.mutate(q)
		w := httptest.NewRecorder()
		r.srv.Handler().ServeHTTP(w, q)
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	// A bad hello is refused.
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testDesktopToken)
	bad, _, err := websocket.DefaultDialer.Dial(r.ws+"/api/v1/captions/provider", h)
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.WriteJSON(map[string]any{"t": "hello", "protocol": 99})
	_ = bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := bad.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("bad hello not refused: %v", err)
	}
	bad.Close()
	// One provider at a time.
	dialProvider(t, r.ws)
	second, _, err := websocket.DefaultDialer.Dial(r.ws+"/api/v1/captions/provider", h)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.WriteJSON(captions.ProviderMessage{T: captions.MsgHello, Protocol: 1})
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := second.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("second provider not refused: %v", err)
	}
}

// Caption host routes are management: denied through Funnel even with the
// host token (the Funnel guardrail test lists them too).
func TestCaptionRoutesDeniedThroughFunnel(t *testing.T) {
	r := newCaptionRig(t)
	for _, path := range []string{"/captions/ingest", "/captions/transcript?format=vtt", "/captions/transcript?format=json"} {
		q := httptest.NewRequest("GET", "https://studio.tail1234.ts.net/api/v1/sessions/"+r.sess.ID+path, nil)
		q.RemoteAddr = "127.0.0.1:40000"
		q.Header.Set("Tailscale-Funnel-Request", "?1")
		q.Header.Set("X-Forwarded-For", "203.0.113.9")
		q.Header.Set("Authorization", "Bearer "+r.sess.HostToken)
		w := httptest.NewRecorder()
		r.srv.Handler().ServeHTTP(w, q)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s through Funnel = %d", path, w.Code)
		}
	}
}
