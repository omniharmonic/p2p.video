package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/integrations"
)

// Post-take ecosystem actions (SL-P4-1, SL-P4-3). Every route is host-only
// through requireHost, so none is reachable through Funnel:
//
//	GET  /api/v1/takes/{id}/integrations          status, engines and capabilities
//	POST /api/v1/takes/{id}/transcript            transcribe with the selected engine (202)
//	POST /api/v1/takes/{id}/transcript/cancel     stop a running transcript
//	PUT  /api/v1/takes/{id}/transcript/engine     choose the engine {"engine":"builtin"|"minutes"} (host-wide)
//	POST /api/v1/takes/{id}/parachute             send the session note (202)
//	POST /api/v1/takes/{id}/handoff/clip-editor   write the clip-editor handoff
var integrationRoutes = []struct{ suffix, method string }{
	{"/integrations", http.MethodGet},
	{"/transcript/cancel", http.MethodPost},
	{"/transcript/engine", http.MethodPut},
	{"/transcript", http.MethodPost},
	{"/parachute", http.MethodPost},
	{"/handoff/clip-editor", http.MethodPost},
}

// handleTakeIntegrations reports whether the path was an integration route.
func (s *Server) handleTakeIntegrations(w http.ResponseWriter, r *http.Request) bool {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/takes/")
	for _, route := range integrationRoutes {
		if !strings.HasSuffix(rest, route.suffix) {
			continue
		}
		takeID := strings.TrimSuffix(rest, route.suffix)
		if takeID == "" || strings.Contains(takeID, "/") {
			return false
		}
		s.serveTakeIntegration(w, r, takeID, route.suffix, route.method)
		return true
	}
	return false
}

func (s *Server) serveTakeIntegration(w http.ResponseWriter, r *http.Request, takeID, action, method string) {
	if r.Method != method {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	take, err := s.Store.GetTake(takeID)
	if err != nil {
		http.Error(w, "take not found", http.StatusNotFound)
		return
	}
	if !s.requireHost(w, r, take.SessionID) {
		return
	}
	if s.Integrations == nil {
		http.Error(w, "integrations unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusAccepted
	switch action {
	case "/integrations":
		writeJSON(w, http.StatusOK, s.Integrations.View(takeID))
		return
	case "/transcript":
		err = s.Integrations.StartTranscript(takeID)
	case "/transcript/cancel":
		err, status = s.Integrations.CancelTranscript(takeID), http.StatusOK
	case "/transcript/engine":
		var body struct {
			Engine string `json:"engine"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body) != nil {
			http.Error(w, "expected {\"engine\": \"builtin\" | \"minutes\"}", http.StatusBadRequest)
			return
		}
		err, status = s.Integrations.SetTranscriptionEngine(body.Engine), http.StatusOK
	case "/parachute":
		err = s.Integrations.StartParachute(takeID)
	case "/handoff/clip-editor":
		err, status = s.Integrations.WriteClipEditor(takeID), http.StatusOK
	}
	switch {
	case err == nil:
		writeJSON(w, status, s.Integrations.View(takeID))
	case errors.Is(err, integrations.ErrUnknownEngine):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, integrations.ErrBusy), errors.Is(err, integrations.ErrNotRunning):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, integrations.ErrUnavailable):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		http.Error(w, err.Error(), http.StatusConflict)
	}
}
