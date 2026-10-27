package httpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/livestream"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

type recordingFFmpeg struct {
	mu    sync.Mutex
	argv  []string
	stdin bytes.Buffer
}

func (f *recordingFFmpeg) launch(argv []string) (*livestream.Process, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	progR, progW := io.Pipe()
	errR, errW := io.Pipe()
	done := make(chan struct{})
	f.mu.Lock()
	f.argv = argv
	f.mu.Unlock()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := inR.Read(buf)
			f.mu.Lock()
			f.stdin.Write(buf[:n])
			f.mu.Unlock()
			if n > 0 {
				_, _ = io.WriteString(progW, "frame=30\nfps=30\nprogress=continue\n")
			}
			if err != nil {
				break
			}
		}
		_ = outW.Close()
		_ = progW.Close()
		_ = errW.Close()
		close(done)
	}()
	return &livestream.Process{Stdin: inW, Stdout: outR, Progress: progR, Stderr: errR,
		Wait: func() error { <-done; return nil }, Kill: func() error { return inR.Close() }}, nil
}

func (f *recordingFFmpeg) received() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stdin.String()
}

func TestLivestreamRoutesHostOnlyAndNeverEchoKeys(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{DataDir: t.TempDir(), PublicURL: "https://studio.tail1234.ts.net", SecretKey: []byte("livestream-test-secret-0123456789")}
	hub := signaling.NewHub(st, cfg, nil)
	defer hub.Close()
	fake := &recordingFFmpeg{}
	srv := &Server{Config: cfg, Store: st, Hub: hub, DesktopToken: testDesktopToken, Livestream: livestream.NewManager(fake.launch, livestream.Options{})}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := adminPost(ts.URL+"/api/v1/sessions", `{"title":"Live"}`)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create: %v %v", err, resp)
	}
	var sess CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	base := ts.URL + "/api/v1/sessions/" + sess.ID + "/livestream"
	const key = "yt-SECRET-stream-key-42"
	body := `{"preset":"720p30","destinations":[{"name":"YouTube","url":"rtmp://a.rtmp.youtube.com/live2","key":"` + key + `"}]}`

	do := func(method, url, token, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(raw), key) {
			t.Fatalf("%s %s echoed the stream key: %s", method, url, raw)
		}
		return resp, string(raw)
	}

	// No token and guest (invite) token are refused on every route.
	for _, token := range []string{"", sess.Token, "garbage"} {
		for _, method := range []string{"GET", "POST", "DELETE"} {
			if r, _ := do(method, base, token, body); r.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s with token %q = %d", method, token, r.StatusCode)
			}
		}
		h := http.Header{}
		if token != "" {
			h.Set("Authorization", "Bearer "+token)
		}
		if c, r, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ingest", h); err == nil {
			c.Close()
			t.Fatal("ingest accepted without host token")
		} else if r == nil || r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("ingest without host token: %v", err)
		}
	}
	otherHost := signaling.MintHostToken("other-session", time.Hour, cfg.SecretKey)
	if r, _ := do("POST", base, otherHost, body); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session's host token = %d", r.StatusCode)
	}

	// Host flow: idle -> start -> ingest bytes -> status -> stop.
	if r, out := do("GET", base, sess.HostToken, ""); r.StatusCode != 200 || !strings.Contains(out, `"state":"idle"`) {
		t.Fatalf("idle status %d %s", r.StatusCode, out)
	}
	if r, out := do("POST", base, sess.HostToken, `{"destinations":[{"name":"x","url":"rtmp://h/app","key":"bad key-with-space"}]}`); r.StatusCode != 400 || strings.Contains(out, "key-with-space") {
		t.Fatalf("unsafe key: %d %s", r.StatusCode, out)
	}
	if r, out := do("POST", base, sess.HostToken, `{"destinations":[],"extra":1}`); r.StatusCode != 400 {
		t.Fatalf("unknown field: %d %s", r.StatusCode, out)
	}
	if r, out := do("POST", base, sess.HostToken, body); r.StatusCode != 200 || !strings.Contains(out, `"server":"rtmp://a.rtmp.youtube.com"`) {
		t.Fatalf("start %d %s", r.StatusCode, out)
	}
	if r, _ := do("POST", base, sess.HostToken, body); r.StatusCode != http.StatusConflict {
		t.Fatalf("second start = %d", r.StatusCode)
	}
	h := http.Header{"Authorization": {"Bearer " + sess.HostToken}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ingest", h)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("webm-header"))
	_ = conn.WriteMessage(websocket.TextMessage, []byte("ignored"))
	_ = conn.WriteMessage(websocket.BinaryMessage, []byte("-cluster"))
	deadline := time.Now().Add(3 * time.Second)
	for fake.received() != "webm-header-cluster" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fake.received() != "webm-header-cluster" {
		t.Fatalf("ffmpeg stdin got %q", fake.received())
	}
	fake.mu.Lock()
	argv := strings.Join(fake.argv, " ")
	fake.mu.Unlock()
	// The encoder never sees a destination or key (they would show in ps).
	if strings.Contains(argv, key) || strings.Contains(argv, "rtmp") || !strings.Contains(argv, "pipe:1") {
		t.Fatalf("encoder argv carries a destination: %s", argv)
	}
	// A second concurrent ingest is refused.
	if c, r, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ingest", h); err == nil {
		c.Close()
		t.Fatal("second ingest accepted")
	} else if r == nil || r.StatusCode != http.StatusConflict {
		t.Fatalf("second ingest: %v", err)
	}
	var status livestream.Status
	for time.Now().Before(deadline) {
		_, out := do("GET", base, sess.HostToken, "")
		_ = json.Unmarshal([]byte(out), &status)
		if status.State == livestream.StateLive {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status.State != livestream.StateLive || status.BytesIn != int64(len("webm-header-cluster")) || !status.IngestConnected {
		t.Fatalf("status %+v", status)
	}
	if r, out := do("DELETE", base, sess.HostToken, ""); r.StatusCode != 200 || !strings.Contains(out, `"state":"stopped"`) {
		t.Fatalf("stop %d %s", r.StatusCode, out)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("ingest socket still open after stop")
	}
	if r, _ := do("DELETE", base, sess.HostToken, ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("double stop = %d", r.StatusCode)
	}
	if c, r, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(base, "http")+"/ingest", h); err == nil {
		c.Close()
		t.Fatal("ingest accepted after stop")
	} else if r == nil || r.StatusCode != http.StatusConflict {
		t.Fatalf("ingest after stop: %v", err)
	}

	// A host without a configured manager reports unavailable, after auth.
	bare := httptest.NewServer((&Server{Config: cfg, Store: st, Hub: hub}).Handler())
	defer bare.Close()
	if r, _ := do("GET", fmt.Sprintf("%s/api/v1/sessions/%s/livestream", bare.URL, sess.ID), sess.HostToken, ""); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured = %d", r.StatusCode)
	}
	if r, _ := do("GET", fmt.Sprintf("%s/api/v1/sessions/%s/livestream", bare.URL, sess.ID), "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unconfigured unauthenticated = %d", r.StatusCode)
	}
}
