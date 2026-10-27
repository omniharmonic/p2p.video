package httpserver

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
)

// handleSubsourceLink mints a phone-camera link for the calling participant
// [SL-P1-9]: POST /api/v1/sessions/{id}/subsource with the participant's own
// signed grant (its upload token) as the bearer. Only an admitted host or
// guest that is in the call right now gets one. Like the guest page, it works
// through Serve and Funnel: it grants nothing beyond a phone of that
// participant, for 10 minutes, once. The token is never logged.
func (s *Server) handleSubsourceLink(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess, err := s.Store.GetSession(sessionID)
	if err != nil || sess.EndedAt != nil {
		http.Error(w, "session unavailable", http.StatusUnauthorized)
		return
	}
	token, exp, err := s.Hub.MintSubsourceCapability(sessionID, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	switch {
	case errors.Is(err, signaling.ErrSubsourceLimit):
		http.Error(w, "This participant already has the maximum number of phone cameras.", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "Only someone in the call can add a phone camera.", http.StatusForbidden)
		return
	}
	q := url.Values{"session": {sessionID}, "token": {token}}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(signaling.SubsourceLinkResponse{
		Path:      "/phone.html?" + q.Encode(),
		BaseURL:   s.phoneBaseURL(r),
		ExpiresAt: exp.UnixMilli(),
	})
}

// phoneBaseURL is where a phone can reach this daemon when the requester is
// on the local dashboard (a phone can't open localhost): the configured HTTPS
// public URL. Empty means "use the page's own origin" (Serve/Funnel guests).
func (s *Server) phoneBaseURL(r *http.Request) string {
	if !localManagement(r) || s.Config == nil || s.Config.PublicURL == "" {
		return ""
	}
	u, err := url.Parse(s.Config.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Hostname() == "localhost" || u.User != nil {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && ip.IsLoopback() {
		return ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host, "/")
}
