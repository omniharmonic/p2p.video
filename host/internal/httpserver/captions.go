package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/captions"
)

// Live captions [SL-P4-2], docs/integrations/CAPTIONS.md.
const (
	captionReadTimeout   = 60 * time.Second
	captionPingInterval  = 15 * time.Second
	captionHelloTimeout  = 5 * time.Second
	captionProviderQueue = 512
)

// desktopAuthorized applies the desktop state endpoint's rules: direct
// loopback only (never Serve/Funnel, no browser Origin) and the data-directory
// desktop token. It writes the error response itself.
func (s *Server) desktopAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if !desktopRequestAllowed(r) {
		http.Error(w, "available only to the local host app", http.StatusForbidden)
		return false
	}
	if s.DesktopToken == "" {
		http.Error(w, "desktop control is unavailable", http.StatusServiceUnavailable)
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.DesktopToken)) != 1 {
		http.Error(w, "desktop authorization required", http.StatusUnauthorized)
		return false
	}
	return true
}

// handleCaptionSession serves host-management caption routes:
//
//	GET /api/v1/sessions/{id}/captions/ingest                 WebSocket of binary PCM frames
//	GET /api/v1/sessions/{id}/captions/transcript?format=vtt  final captions (vtt or json)
//
// Both require the host token AND managementAllowed (requireHost), so they are
// never reachable through Funnel.
func (s *Server) handleCaptionSession(w http.ResponseWriter, r *http.Request, sessID, sub string) {
	if !s.requireHost(w, r, sessID) {
		return
	}
	if s.Captions == nil {
		http.Error(w, "live captions are not available on this host", http.StatusServiceUnavailable)
		return
	}
	switch sub {
	case "ingest":
		s.handleCaptionIngest(w, r, sessID)
	case "transcript":
		s.handleCaptionTranscript(w, r, sessID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleCaptionIngest(w http.ResponseWriter, r *http.Request, sessID string) {
	if sess, err := s.Store.GetSession(sessID); err != nil || sess.EndedAt != nil {
		http.Error(w, "session unavailable", http.StatusNotFound)
		return
	}
	release, err := s.Captions.OpenIngest(sessID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	defer release()
	upgrader := websocket.Upgrader{CheckOrigin: s.checkOrigin, ReadBufferSize: 16 * 1024}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("caption ingest upgrade failed", "session", sessID, "err", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(captions.MaxFrameBytes)
	_ = conn.SetReadDeadline(time.Now().Add(captionReadTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(captionReadTimeout)) })
	done := make(chan struct{})
	defer close(done)
	go pingLoop(conn, done)
	var rejected int
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(captionReadTimeout))
		if kind != websocket.BinaryMessage {
			continue
		}
		if err := s.Captions.Ingest(sessID, data); err != nil {
			rejected++
		}
	}
	slog.Info("caption ingest closed", "session", sessID, "rejectedFrames", rejected)
}

func (s *Server) handleCaptionTranscript(w http.ResponseWriter, r *http.Request, sessID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t, ok := s.Captions.Transcript(sessID)
	if !ok {
		t = captions.Transcript{SessionID: sessID, Source: "live-captions", Segments: []captions.Segment{}}
	}
	w.Header().Set("Cache-Control", "no-store")
	name := "captions-" + sanitizeFilename(sessID)
	switch r.URL.Query().Get("format") {
	case "", "vtt":
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.vtt"`, name))
		_, _ = w.Write([]byte(t.VTT()))
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.json"`, name))
		_ = json.NewEncoder(w).Encode(t)
	default:
		http.Error(w, "format must be vtt or json", http.StatusBadRequest)
	}
}

func sanitizeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

// providerSocket adapts a provider WebSocket to captions.Provider with a
// bounded, non-blocking send queue drained by one writer goroutine.
type providerSocket struct {
	conn  *websocket.Conn
	queue chan providerFrame
	done  chan struct{}
}

type providerFrame struct {
	kind int
	data []byte
}

func (p *providerSocket) send(kind int, b []byte) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.queue <- providerFrame{kind, b}:
		return true
	default:
		return false
	}
}
func (p *providerSocket) SendText(b []byte) bool   { return p.send(websocket.TextMessage, b) }
func (p *providerSocket) SendBinary(b []byte) bool { return p.send(websocket.BinaryMessage, b) }
func (p *providerSocket) Close()                   { _ = p.conn.Close() }

// handleCaptionProvider accepts the on-device recognizer (normally the Mac
// app): GET /api/v1/captions/provider, a WebSocket on direct loopback with
// the desktop token. One provider at a time.
func (s *Server) handleCaptionProvider(w http.ResponseWriter, r *http.Request) {
	if !s.desktopAuthorized(w, r) {
		return
	}
	if s.Captions == nil {
		http.Error(w, "live captions are not available on this host", http.StatusServiceUnavailable)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("caption provider upgrade failed", "err", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(64 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(captionHelloTimeout))
	kind, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	hello, err := captions.ParseProviderHello(raw)
	if err != nil || kind != websocket.TextMessage {
		closeWS(conn, websocket.ClosePolicyViolation, captions.ErrProviderHello.Error())
		return
	}
	sock := &providerSocket{conn: conn, queue: make(chan providerFrame, captionProviderQueue), done: make(chan struct{})}
	defer close(sock.done)
	if err := s.Captions.AttachProvider(sock, hello); err != nil {
		closeWS(conn, websocket.ClosePolicyViolation, err.Error())
		return
	}
	defer s.Captions.DetachProvider(sock)
	go func() {
		t := time.NewTicker(captionPingInterval)
		defer t.Stop()
		for {
			select {
			case <-sock.done:
				return
			case f := <-sock.queue:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if conn.WriteMessage(f.kind, f.data) != nil {
					_ = conn.Close()
					return
				}
			case <-t.C:
				if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
					_ = conn.Close()
					return
				}
			}
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(captionReadTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(captionReadTimeout)) })
	for {
		kind, raw, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if !errors.As(err, &ce) {
				slog.Info("caption provider read ended", "err", err)
			}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(captionReadTimeout))
		if kind == websocket.TextMessage {
			s.Captions.HandleProviderMessage(sock, raw)
		}
	}
}

func pingLoop(conn *websocket.Conn, done <-chan struct{}) {
	t := time.NewTicker(captionPingInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return
			}
		}
	}
}

func closeWS(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}
