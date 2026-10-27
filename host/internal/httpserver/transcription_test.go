package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/filespeech"
	"github.com/omniharmonic/studiolink/host/internal/integrations"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

type transcriptionRig struct {
	srv    *Server
	ts     *httptest.Server
	svc    *integrations.Service
	broker *filespeech.Broker
	host   string
	pkg    string
	ws     string
}

func newTranscriptionRig(t *testing.T) *transcriptionRig {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{DataDir: root, SecretKey: []byte("transcription-route-secret-0123456789")}
	if err := st.CreateSession(&store.Session{ID: "s1", Title: "Pilot", CreatedAt: time.Now(), PresetJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateTake(&store.Take{ID: "take1", SessionID: "s1", Number: 1, Status: "complete", WallClockStart: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "exports", "Pilot_s1", "take_01_take1")
	if err := os.MkdirAll(filepath.Join(pkg, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := export.WriteManifest(pkg, "s1", "Pilot", 1, time.Now(), 30, []export.ExportSource{
		{ID: "a", Participant: "Ana", ParticipantID: "p1", Kind: "audio", FileRelPath: "audio/Ana.wav", DurationSec: 8, OffsetSec: 0.5},
		{ID: "b", Participant: "Ben", ParticipantID: "p2", Kind: "audio", FileRelPath: "audio/Ben.wav", DurationSec: 8, OffsetSec: 1},
	}); err != nil {
		t.Fatal(err)
	}
	pipe := postproc.NewPipeline(st, cfg, nil)
	broker := filespeech.NewBroker()
	svc := &integrations.Service{Store: st, Package: pipe, Builtin: broker, MinutesReason: "minutes-cli not found", StateDir: filepath.Join(root, "integrations")}
	t.Cleanup(svc.Close)
	srv := &Server{Config: cfg, Store: st, Pipeline: pipe, Integrations: svc, Transcription: broker, DesktopToken: testDesktopToken}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &transcriptionRig{srv: srv, ts: ts, svc: svc, broker: broker, host: signaling.MintHostToken("s1", time.Hour, cfg.SecretKey), pkg: pkg,
		ws: "ws" + strings.TrimPrefix(ts.URL, "http")}
}

func (r *transcriptionRig) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, r.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+r.host)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

func (r *transcriptionRig) view(t *testing.T) integrations.View {
	t.Helper()
	code, body := r.do(t, "GET", "/api/v1/takes/take1/integrations", "")
	var v integrations.View
	if code != 200 || json.Unmarshal([]byte(body), &v) != nil {
		t.Fatalf("view %d %s", code, body)
	}
	return v
}

func (r *transcriptionRig) waitState(t *testing.T, want string) integrations.View {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := r.view(t)
		if v.Transcript.State == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("transcript state %q, want %q: %+v", v.Transcript.State, want, v.Transcript)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// dialTranscriber connects a provider like the Mac app does.
func dialTranscriber(t *testing.T, base, state string) *websocket.Conn {
	t.Helper()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testDesktopToken)
	conn, resp, err := websocket.DefaultDialer.Dial(base+"/api/v1/transcription/provider", h)
	if err != nil {
		t.Fatalf("provider dial: %v %v", err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.WriteJSON(filespeech.Message{T: filespeech.MsgHello, Protocol: 1, Name: "Fake", Engine: "fake-asr", Languages: []string{"en-US"}, State: state})
	var ready filespeech.Message
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil || ready.T != filespeech.MsgReady {
		t.Fatalf("no ready: %+v %v", ready, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	return conn
}

func readJob(t *testing.T, conn *websocket.Conn) filespeech.Message {
	t.Helper()
	var m filespeech.Message
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuiltinTranscriptionEndToEndOverTheProviderSocket(t *testing.T) {
	r := newTranscriptionRig(t)
	// No app yet: the engine is selected but says what to do.
	v := r.view(t)
	if v.Transcription.Engine != "builtin" || v.Capabilities.Transcript.Available || !strings.Contains(v.Capabilities.Transcript.Reason, "Open p2p.video") {
		t.Fatalf("before provider: %+v %+v", v.Transcription, v.Capabilities.Transcript)
	}
	if code, body := r.do(t, "POST", "/api/v1/takes/take1/transcript", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "Open p2p.video") {
		t.Fatalf("start without provider = %d %s", code, body)
	}
	conn := dialTranscriber(t, r.ws, filespeech.StateReady)
	if v := r.view(t); !v.Capabilities.Transcript.Available || !strings.Contains(v.Capabilities.Transcript.Target, "fake-asr") {
		t.Fatalf("after provider: %+v", v.Capabilities.Transcript)
	}
	if code, body := r.do(t, "POST", "/api/v1/takes/take1/transcript", ""); code != http.StatusAccepted {
		t.Fatalf("start = %d %s", code, body)
	}
	for i := 0; i < 2; i++ {
		job := readJob(t, conn)
		if job.T != filespeech.MsgJob || !strings.HasPrefix(job.Path, r.pkg) || job.Language != "en-US" || job.DurationSec != 8 {
			t.Fatalf("job %+v", job)
		}
		name := strings.TrimSuffix(filepath.Base(job.Path), ".wav")
		_ = conn.WriteJSON(filespeech.Message{T: filespeech.MsgProgress, JobID: job.JobID, ProcessedSec: 4})
		_ = conn.WriteJSON(filespeech.Message{T: filespeech.MsgSegment, JobID: job.JobID, Segment: &filespeech.Segment{Start: 1, End: 2, Text: "I am " + name,
			Words: []filespeech.Word{{Word: "I", Start: 1, End: 1.2, Confidence: 0.9}}}})
		_ = conn.WriteJSON(filespeech.Message{T: filespeech.MsgDone, JobID: job.JobID, Locale: "en-US", ASR: "fake-asr"})
	}
	v = r.waitState(t, "done")
	if !v.HasTranscript || !v.HasHandoff {
		t.Fatalf("view %+v", v)
	}
	doc, err := transcript.Read(r.pkg)
	if err != nil || len(doc.Segments) != 2 || doc.Segments[0].Speaker != "Ana" || doc.Segments[0].Start != 1.5 || doc.Segments[1].Text != "I am Ben" || doc.Engine.Name != "studiolink-builtin" {
		t.Fatalf("doc %+v %v", doc, err)
	}
}

func TestTranscriptCancelAndEngineRoutes(t *testing.T) {
	r := newTranscriptionRig(t)
	conn := dialTranscriber(t, r.ws, filespeech.StateReady)
	if code, _ := r.do(t, "POST", "/api/v1/takes/take1/transcript/cancel", ""); code != http.StatusConflict {
		t.Fatalf("cancel idle = %d", code)
	}
	if code, _ := r.do(t, "POST", "/api/v1/takes/take1/transcript", ""); code != http.StatusAccepted {
		t.Fatal(code)
	}
	job := readJob(t, conn)
	if code, body := r.do(t, "POST", "/api/v1/takes/take1/transcript/cancel", ""); code != http.StatusOK {
		t.Fatalf("cancel = %d %s", code, body)
	}
	cancel := readJob(t, conn)
	if cancel.T != filespeech.MsgCancel || cancel.JobID != job.JobID {
		t.Fatalf("cancel message %+v", cancel)
	}
	_ = conn.WriteJSON(filespeech.Message{T: filespeech.MsgError, JobID: job.JobID, Code: "cancelled"})
	r.waitState(t, "cancelled")
	if _, err := os.Stat(filepath.Join(r.pkg, "transcript")); !os.IsNotExist(err) {
		t.Fatal("cancel wrote a transcript")
	}
	// Engine choice: Minutes is refused when not detected; bad input is 400.
	if code, body := r.do(t, "PUT", "/api/v1/takes/take1/transcript/engine", `{"engine":"minutes"}`); code != http.StatusServiceUnavailable || !strings.Contains(body, "optional") {
		t.Fatalf("minutes = %d %s", code, body)
	}
	if code, _ := r.do(t, "PUT", "/api/v1/takes/take1/transcript/engine", `{"engine":"cloud"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown engine = %d", code)
	}
	if code, _ := r.do(t, "PUT", "/api/v1/takes/take1/transcript/engine", `nope`); code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", code)
	}
	if code, body := r.do(t, "PUT", "/api/v1/takes/take1/transcript/engine", `{"engine":"builtin"}`); code != http.StatusOK || !strings.Contains(body, `"engine":"builtin"`) {
		t.Fatalf("builtin = %d %s", code, body)
	}
	if code, _ := r.do(t, "GET", "/api/v1/takes/take1/transcript/engine", ""); code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method = %d", code)
	}
}

func TestProviderDisconnectAndPermissionAreExplicit(t *testing.T) {
	r := newTranscriptionRig(t)
	denied := dialTranscriber(t, r.ws, filespeech.StateNotAuthorized)
	if v := r.view(t); v.Capabilities.Transcript.Code != "not_authorized" || !strings.Contains(v.Capabilities.Transcript.Reason, "Speech Recognition") {
		t.Fatalf("permission: %+v", v.Capabilities.Transcript)
	}
	// The user allows Speech Recognition in the app: it re-announces.
	_ = denied.WriteJSON(filespeech.Message{T: filespeech.MsgStatus, State: filespeech.StateReady})
	deadline := time.Now().Add(3 * time.Second)
	for !r.view(t).Capabilities.Transcript.Available {
		if time.Now().After(deadline) {
			t.Fatal("status update not applied")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code, _ := r.do(t, "POST", "/api/v1/takes/take1/transcript", ""); code != http.StatusAccepted {
		t.Fatal(code)
	}
	readJob(t, denied)
	denied.Close() // the app quits mid-job
	v := r.waitState(t, "error")
	if !strings.Contains(v.Transcript.Message, "disconnected") {
		t.Fatalf("%+v", v.Transcript)
	}
}

// The provider socket follows the caption provider's rules: direct loopback,
// desktop token, no browser Origin, never through Serve/Funnel, one at a time.
func TestTranscriptionProviderAuthorization(t *testing.T) {
	r := newTranscriptionRig(t)
	cases := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"no token", func(*http.Request) {}, http.StatusUnauthorized},
		{"host token", func(q *http.Request) { q.Header.Set("Authorization", "Bearer "+r.host) }, http.StatusUnauthorized},
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
		{"browser origin", func(q *http.Request) {
			q.Header.Set("Authorization", "Bearer "+testDesktopToken)
			q.Header.Set("Origin", "http://127.0.0.1:7443")
		}, http.StatusForbidden},
	}
	for _, tc := range cases {
		q := httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/transcription/provider", nil)
		q.RemoteAddr = "127.0.0.1:40000"
		tc.mutate(q)
		w := httptest.NewRecorder()
		r.srv.Handler().ServeHTTP(w, q)
		if w.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, w.Code, tc.want)
		}
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+testDesktopToken)
	bad, _, err := websocket.DefaultDialer.Dial(r.ws+"/api/v1/transcription/provider", h)
	if err != nil {
		t.Fatal(err)
	}
	_ = bad.WriteJSON(map[string]any{"t": "hello", "protocol": 99})
	_ = bad.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := bad.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("bad hello not refused: %v", err)
	}
	bad.Close()
	dialTranscriber(t, r.ws, filespeech.StateReady)
	second, _, err := websocket.DefaultDialer.Dial(r.ws+"/api/v1/transcription/provider", h)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	_ = second.WriteJSON(filespeech.Message{T: filespeech.MsgHello, Protocol: 1})
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := second.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("second provider not refused: %v", err)
	}
	// Without a broker the socket answers 503.
	none := &Server{Config: r.srv.Config, Store: r.srv.Store, DesktopToken: testDesktopToken}
	q := httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/transcription/provider", nil)
	q.RemoteAddr = "127.0.0.1:40000"
	q.Header.Set("Authorization", "Bearer "+testDesktopToken)
	w := httptest.NewRecorder()
	none.Handler().ServeHTTP(w, q)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no broker = %d", w.Code)
	}
}
