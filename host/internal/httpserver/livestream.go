package httpserver

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/livestream"
)

const (
	livestreamMaxFrame    = 16 << 20 // one MediaRecorder timeslice
	livestreamReadTimeout = 30 * time.Second
)

// handleLivestream serves host-management livestream routes:
//
//	GET    /api/v1/sessions/{id}/livestream         status
//	POST   /api/v1/sessions/{id}/livestream         start {destinations, preset}
//	DELETE /api/v1/sessions/{id}/livestream         stop
//	GET    /api/v1/sessions/{id}/livestream/ingest  WebSocket of binary media
//
// All require the host token AND managementAllowed (requireHost), so they are
// never reachable through Funnel. Responses never include stream keys.
func (s *Server) handleLivestream(w http.ResponseWriter, r *http.Request, sessID, sub string) {
	if !s.requireHost(w, r, sessID) {
		return
	}
	if s.Livestream == nil {
		http.Error(w, "livestreaming is not configured on this host", http.StatusServiceUnavailable)
		return
	}
	switch sub {
	case "":
	case "ingest":
		s.handleLivestreamIngest(w, r, sessID)
		return
	default:
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		st, _ := s.Livestream.Status(sessID)
		writeJSON(w, http.StatusOK, st)
	case http.MethodPost:
		sess, err := s.Store.GetSession(sessID)
		if err != nil || sess.EndedAt != nil {
			http.Error(w, "session unavailable", http.StatusNotFound)
			return
		}
		var req livestream.StartRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		st, err := s.Livestream.Start(sessID, req)
		if err != nil {
			livestreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	case http.MethodDelete:
		st, err := s.Livestream.Stop(sessID)
		if err != nil {
			livestreamError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleLivestreamIngest(w http.ResponseWriter, r *http.Request, sessID string) {
	if st, _ := s.Livestream.Status(sessID); st.State == "idle" || st.State == livestream.StateStopped || st.State == livestream.StateFailed {
		http.Error(w, livestream.ErrNotActive.Error(), http.StatusConflict)
		return
	} else if st.IngestConnected {
		http.Error(w, livestream.ErrIngestBusy.Error(), http.StatusConflict)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: s.checkOrigin, ReadBufferSize: 64 * 1024}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("livestream ingest upgrade failed", "session", sessID, "err", err)
		return
	}
	defer conn.Close()
	conn.SetReadLimit(livestreamMaxFrame)
	next := func() ([]byte, error) {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(livestreamReadTimeout))
			kind, data, err := conn.ReadMessage()
			if err != nil {
				return nil, err
			}
			if kind == websocket.BinaryMessage {
				return data, nil
			}
		}
	}
	err = s.Livestream.Ingest(sessID, next, func() { _ = conn.Close() })
	reason := "livestream ended"
	if err != nil {
		reason = err.Error()
	}
	_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason), time.Now().Add(time.Second))
}

func livestreamError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, livestream.ErrInvalidRequest):
		code = http.StatusBadRequest
	case errors.Is(err, livestream.ErrAlreadyActive), errors.Is(err, livestream.ErrIngestBusy):
		code = http.StatusConflict
	case errors.Is(err, livestream.ErrNotActive):
		code = http.StatusNotFound
	case errors.Is(err, livestream.ErrUnavailable):
		code = http.StatusServiceUnavailable
	}
	// livestream errors are constructed without URLs or keys.
	http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
