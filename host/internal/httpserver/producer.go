package httpserver

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Producer links [SL-P1-11]. An instant meeting has its own producer link;
// a meeting opened from a room uses the room's, so it survives from episode
// to episode like the guest link. The secret is
//
//	hex(HMAC-SHA256(daemon key, "studiolink/producer-invite/v1" ‖ scope ‖ nonce))
//
// (domain-separated from room guest links), and only the nonce and the
// secret's SHA-256 are stored. The link reaches the producer's waiting room
// and the session detail an invite may read; it is never a host credential,
// so every host HTTP route (requireHost: host token + management position)
// refuses it, and so does admin management (desktop token / admin cookie).

func (s *Server) producerSecret(scope, nonce string) string {
	m := hmac.New(sha256.New, s.Config.SecretKey)
	m.Write([]byte("studiolink/producer-invite/v1\x00" + scope + "\x00" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

// rotateProducerLink issues a new link for scope (the old one stops working).
func (s *Server) rotateProducerLink(scope string) (oldNonce string, err error) {
	oldNonce, _, err = s.Store.ProducerLink(scope)
	if err != nil {
		return "", err
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("producer link entropy: %w", err)
	}
	nonce := hex.EncodeToString(b)
	return oldNonce, s.Store.SetProducerLink(scope, nonce, hashToken(s.producerSecret(scope, nonce)))
}

// producerScope is the scope whose producer link admits producers to sess.
func producerScope(sess *store.Session) string {
	if sess.RoomID != "" {
		return store.RoomProducerScope(sess.RoomID)
	}
	return store.SessionProducerScope(sess.ID)
}

// producerCredential reports whether token is sess's current producer link
// (an archived room's link admits nobody) and names it for JoinCredential.
func (s *Server) producerCredential(sess *store.Session, token string) (string, bool) {
	if token == "" || sess == nil {
		return "", false
	}
	if sess.RoomID != "" {
		room, err := s.Store.GetRoom(sess.RoomID)
		if err != nil || room.Archived {
			return "", false
		}
	}
	nonce, hash, err := s.Store.ProducerLink(producerScope(sess))
	if err != nil || nonce == "" || !tokenHashMatches(token, hash) {
		return "", false
	}
	return producerCredentialName(nonce), true
}

func producerCredentialName(nonce string) string {
	if nonce == "" {
		return ""
	}
	return "producer:" + nonce
}

// producerLinkView renders scope's link. join is the query that selects the
// meeting (session=<id> or room=<slug>).
func (s *Server) producerLinkView(scope string, join url.Values, base guestBaseFn) signaling.ProducerLinkResponse {
	nonce, hash, err := s.Store.ProducerLink(scope)
	if err != nil || nonce == "" {
		return signaling.ProducerLinkResponse{}
	}
	secret := s.producerSecret(scope, nonce)
	if hashToken(secret) != hash {
		return signaling.ProducerLinkResponse{} // daemon key replaced: rotate for a new link
	}
	q := url.Values{}
	for k, v := range join {
		q[k] = v
	}
	q.Set("token", secret)
	v := signaling.ProducerLinkResponse{Enabled: true, Path: "/join?" + q.Encode()}
	b, remote := base()
	v.URL, v.Remote = b+v.Path, remote
	return v
}

// handleSessionProducer serves GET /api/v1/sessions/{id}/producer and POST
// .../producer/{rotate,revoke}: host token from a management position only.
func (s *Server) handleSessionProducer(w http.ResponseWriter, r *http.Request, sessID, action string) {
	if !s.requireHost(w, r, sessID) {
		return
	}
	sess, err := s.Store.GetSession(sessID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	scope := producerScope(sess)
	join := url.Values{"session": {sess.ID}}
	resp := signaling.ProducerLinkResponse{}
	if sess.RoomID != "" {
		room, err := s.Store.GetRoom(sess.RoomID)
		if err != nil {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		join = url.Values{"room": {room.Slug}}
		resp.RoomID = room.ID
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
	case action == "rotate" && r.Method == http.MethodPost:
		old, err := s.rotateProducerLink(scope)
		if err != nil {
			http.Error(w, "cannot issue a producer link", http.StatusInternalServerError)
			return
		}
		slog.Info("Producer link rotated", "sessionId", sess.ID, "roomId", sess.RoomID)
		if s.Hub != nil && old != "" {
			s.Hub.DenyPendingWithCredential(sess.ID, producerCredentialName(old))
		}
	case action == "revoke" && r.Method == http.MethodPost:
		old, _, err := s.Store.ProducerLink(scope)
		if err == nil {
			err = s.Store.SetProducerLink(scope, "", "")
		}
		if err != nil {
			http.Error(w, "cannot revoke the producer link", http.StatusInternalServerError)
			return
		}
		slog.Info("Producer link revoked", "sessionId", sess.ID, "roomId", sess.RoomID)
		if s.Hub != nil && old != "" {
			s.Hub.DenyPendingWithCredential(sess.ID, producerCredentialName(old))
		}
	default:
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	view := s.producerLinkView(scope, join, s.memoBase(r))
	view.RoomID = resp.RoomID
	writeSecretJSON(w, http.StatusOK, view)
}

// handleRoomProducer serves POST /api/v1/rooms/{id}/producer/{rotate,revoke}
// (admin management, like the room's guest link).
func (s *Server) handleRoomProducer(w http.ResponseWriter, r *http.Request, room *store.Room, action string) {
	if r.Method != http.MethodPost || (action != "rotate" && action != "revoke") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	scope := store.RoomProducerScope(room.ID)
	var old string
	var err error
	if action == "rotate" {
		old, err = s.rotateProducerLink(scope)
	} else if old, _, err = s.Store.ProducerLink(scope); err == nil {
		err = s.Store.SetProducerLink(scope, "", "")
	}
	if err != nil {
		http.Error(w, "cannot change the producer link", http.StatusInternalServerError)
		return
	}
	slog.Info("Room producer link changed", "roomId", room.ID, "action", action)
	if s.Hub != nil && old != "" {
		if live, err := s.Store.LiveRoomSession(room.ID); err == nil {
			s.Hub.DenyPendingWithCredential(live.ID, producerCredentialName(old))
		}
	}
	writeSecretJSON(w, http.StatusOK, s.roomView(room, s.memoBase(r)))
}
