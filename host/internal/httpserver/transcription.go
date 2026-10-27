package httpserver

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"github.com/omniharmonic/studiolink/host/internal/filespeech"
)

// Built-in post-take transcription [SL-P4-3], docs/integrations/TRANSCRIPTS.md.
const (
	transcriptionHelloTimeout = 5 * time.Second
	transcriptionReadTimeout  = 60 * time.Second
	transcriptionQueue        = 64
)

// transcriptionSocket adapts a provider WebSocket to filespeech.Conn with a
// bounded, non-blocking send queue drained by one writer goroutine.
type transcriptionSocket struct {
	conn  *websocket.Conn
	queue chan []byte
	done  chan struct{}
}

func (p *transcriptionSocket) Send(b []byte) bool {
	select {
	case <-p.done:
		return false
	default:
	}
	select {
	case p.queue <- b:
		return true
	default:
		return false
	}
}

func (p *transcriptionSocket) Close() { _ = p.conn.Close() }

// handleTranscriptionProvider accepts the on-device file recognizer
// (normally the Mac app): GET /api/v1/transcription/provider, a WebSocket on
// direct loopback with the desktop token (the caption provider's rules). One
// provider at a time. Job paths and recognized text are never logged.
func (s *Server) handleTranscriptionProvider(w http.ResponseWriter, r *http.Request) {
	if !s.desktopAuthorized(w, r) {
		return
	}
	if s.Transcription == nil {
		http.Error(w, "built-in transcription is not available on this host", http.StatusServiceUnavailable)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("transcription provider upgrade failed", "err", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(filespeech.MaxMessageBytes)
	_ = conn.SetReadDeadline(time.Now().Add(transcriptionHelloTimeout))
	kind, raw, err := conn.ReadMessage()
	if err != nil {
		return
	}
	hello, err := filespeech.ParseHello(raw)
	if err != nil || kind != websocket.TextMessage {
		closeWS(conn, websocket.ClosePolicyViolation, filespeech.ErrHello.Error())
		return
	}
	sock := &transcriptionSocket{conn: conn, queue: make(chan []byte, transcriptionQueue), done: make(chan struct{})}
	defer close(sock.done)
	if err := s.Transcription.Attach(sock, hello); err != nil {
		closeWS(conn, websocket.ClosePolicyViolation, err.Error())
		return
	}
	defer s.Transcription.Detach(sock)
	slog.Info("Transcription provider connected", "engine", hello.Engine, "state", hello.State)
	go func() {
		t := time.NewTicker(captionPingInterval)
		defer t.Stop()
		for {
			select {
			case <-sock.done:
				return
			case b := <-sock.queue:
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if conn.WriteMessage(websocket.TextMessage, b) != nil {
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
	_ = conn.SetReadDeadline(time.Now().Add(transcriptionReadTimeout))
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(transcriptionReadTimeout)) })
	for {
		kind, raw, err := conn.ReadMessage()
		if err != nil {
			var ce *websocket.CloseError
			if !errors.As(err, &ce) {
				slog.Info("transcription provider read ended", "err", err)
			}
			slog.Info("Transcription provider disconnected")
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(transcriptionReadTimeout))
		if kind == websocket.TextMessage {
			s.Transcription.Handle(sock, raw)
		}
	}
}
