package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/buildinfo"
	"github.com/omniharmonic/studiolink/host/internal/livestream"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
)

// recentTakeLimit bounds the take history returned to the desktop app.
const recentTakeLimit = 10

// DesktopState is the native host app's view of the daemon (SL-P2-1). It is
// served only on direct loopback with the data-directory desktop token, and
// carries no session, invite, host or TURN credentials.
type DesktopState struct {
	Product        string           `json:"product"`
	BuildID        string           `json:"buildId"`
	PID            int              `json:"pid"`
	Recording      bool             `json:"recording"`
	Live           bool             `json:"live"`
	UploadsPending int              `json:"uploadsPending"`
	WaitingCount   int              `json:"waitingCount"`
	Sessions       []DesktopSession `json:"sessions"`
	Takes          []DesktopTake    `json:"takes"`
	// Rooms lists persistent rooms for quick-open menu entries: names and
	// IDs only, never the guest link [SL-P1-5].
	Rooms []DesktopRoom `json:"rooms"`
}

// DesktopRoom is a persistent room as the menu bar shows it.
type DesktopRoom struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Live is true while the room has a meeting open.
	Live bool `json:"live"`
}

// DesktopSession is a session with at least one connected socket.
type DesktopSession struct {
	signaling.DesktopSession
	Title string `json:"title"`
	Live  bool   `json:"live"`
}

// DesktopTake summarizes upload and export progress for a recent take.
type DesktopTake struct {
	ID                  string `json:"id"`
	SessionID           string `json:"sessionId"`
	Number              int    `json:"number"`
	Status              string `json:"status"`
	Tracks              int    `json:"tracks"`
	TracksUploaded      int    `json:"tracksUploaded"`
	MissingParticipants int    `json:"missingParticipants"`
	UploadsComplete     bool   `json:"uploadsComplete"`
	Exported            bool   `json:"exported"`
}

// desktopRequestAllowed admits only a direct loopback request with no proxy,
// Serve or Funnel markers and no browser Origin. Funnel and Serve traffic
// reaches the daemon from loopback too, so the headers are checked explicitly.
func desktopRequestAllowed(r *http.Request) bool {
	if !localManagement(r) || r.Header.Get("Origin") != "" {
		return false
	}
	// The Host header itself must name loopback, whatever the request URL says.
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return false
	}
	for _, h := range []string{"Tailscale-Funnel-Request", "Tailscale-User-Login", "Tailscale-User-Name", "X-Forwarded-Host", "X-Forwarded-Proto"} {
		if r.Header.Get(h) != "" {
			return false
		}
	}
	return true
}

func (s *Server) handleDesktopState(w http.ResponseWriter, r *http.Request) {
	if !desktopRequestAllowed(r) {
		http.Error(w, "desktop state is available only to the local host app", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.DesktopToken == "" {
		http.Error(w, "desktop control is unavailable", http.StatusServiceUnavailable)
		return
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.DesktopToken)) != 1 {
		http.Error(w, "desktop authorization required", http.StatusUnauthorized)
		return
	}
	state, err := s.desktopState()
	if err != nil {
		slog.Error("desktop state failed", "err", err)
		http.Error(w, "desktop state unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(state)
}

func (s *Server) desktopState() (*DesktopState, error) {
	state := &DesktopState{
		Product:  buildinfo.Product,
		BuildID:  buildinfo.ID,
		PID:      os.Getpid(),
		Sessions: []DesktopSession{},
		Takes:    []DesktopTake{},
		Rooms:    []DesktopRoom{},
	}
	connected := map[string]bool{}
	if s.Hub != nil {
		for _, snap := range s.Hub.DesktopSnapshot() {
			ds := DesktopSession{DesktopSession: snap}
			if sess, err := s.Store.GetSession(snap.ID); err == nil {
				ds.Title = sess.Title
			}
			if s.Livestream != nil {
				if st, ok := s.Livestream.Status(snap.ID); ok && liveState(st.State) {
					ds.Live = true
				}
			}
			state.Recording = state.Recording || ds.Recording
			state.Live = state.Live || ds.Live
			state.WaitingCount += len(ds.Waiting)
			state.UploadsPending += ds.UploadingTracks
			connected[snap.ID] = true
			state.Sessions = append(state.Sessions, ds)
		}
	}
	if s.Livestream != nil && !state.Live {
		// A stream can outlive its browser sockets during the reconnect grace.
		state.Live = s.Livestream.AnyActive()
	}
	if s.Store == nil {
		return state, nil
	}
	rooms, err := s.Store.ListRooms(false)
	if err != nil {
		return nil, err
	}
	for _, room := range rooms {
		_, liveErr := s.Store.LiveRoomSession(room.ID)
		state.Rooms = append(state.Rooms, DesktopRoom{ID: room.ID, Name: room.Name, Live: liveErr == nil})
	}
	takes, err := s.Store.ListRecentTakes(recentTakeLimit)
	if err != nil {
		return nil, err
	}
	for _, tk := range takes {
		tracks, err := s.Store.ListTracksByTake(tk.ID)
		if err != nil {
			return nil, err
		}
		missing, err := s.Store.ListMissingTakeParticipants(tk.ID)
		if err != nil {
			return nil, err
		}
		dt := DesktopTake{ID: tk.ID, SessionID: tk.SessionID, Number: tk.Number, Status: tk.Status, Tracks: len(tracks), MissingParticipants: len(missing), Exported: tk.Status == "complete"}
		for _, tr := range tracks {
			if tr.Status == "assembled" || tr.Status == "processed" {
				dt.TracksUploaded++
			}
		}
		dt.UploadsComplete = tk.Status != "recording" && len(missing) == 0 && dt.Tracks > 0 && dt.TracksUploaded == dt.Tracks
		// Uploads still owed to a session whose participants are connected.
		if connected[tk.SessionID] && tk.Status != "recording" && !dt.UploadsComplete && !dt.Exported {
			state.UploadsPending += dt.Tracks - dt.TracksUploaded
		}
		state.Takes = append(state.Takes, dt)
	}
	return state, nil
}

func liveState(state string) bool {
	switch state {
	case livestream.StateWaitingIngest, livestream.StateStarting, livestream.StateLive, livestream.StateReconnecting:
		return true
	}
	return false
}
