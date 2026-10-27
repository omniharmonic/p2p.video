package httpserver

import (
	"bytes"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestManagementAuthorization(t *testing.T) {
	srv, hub, st, cfg, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", Title: "Private", CreatedAt: time.Now()})
	_ = st.CreateTake(&store.Take{ID: "take", SessionID: "room", Number: 1, WallClockStart: time.Now()})
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/v1/sessions/room", ""}, {"POST", "/api/v1/sessions/room/settings", `{"target_fps":60}`}, {"POST", "/api/v1/takes/take/postprocess", ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != 401 {
				t.Errorf("unauthenticated status=%d, want401", w.Code)
			}
		})
	}
	req := httptest.NewRequest("POST", "http://localhost:7443/api/v1/sessions", bytes.NewBufferString(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Origin", "https://attacker.example")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("cross-origin bootstrap=%d, want403", w.Code)
	}
	req = httptest.NewRequest("POST", "https://studio.example.ts.net/api/v1/sessions", bytes.NewBufferString(`{}`))
	req.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("public bootstrap=%d, want403", w.Code)
	}
	hostToken := signaling.MintHostToken("room", time.Hour, cfg.SecretKey)
	req = httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/sessions/room", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+hostToken)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("authorized read=%d", w.Code)
	}
	// L3: a host token is honoured only where host control is allowed: not
	// from another machine, not through private Serve without the owner
	// login, not through Funnel.
	for name, mutate := range map[string]func(*http.Request){
		"remote":        func(r *http.Request) { r.RemoteAddr = "192.0.2.10:5555" },
		"private serve": func(r *http.Request) { r.Host = "studio.example.ts.net"; r.Header.Set("X-Forwarded-For", "100.64.0.9") },
		"funnel":        func(r *http.Request) { r.Header.Set("Tailscale-Funnel-Request", "?1") },
	} {
		req = httptest.NewRequest("GET", "http://127.0.0.1:7443/api/v1/sessions/room", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer "+hostToken)
		mutate(req)
		w = httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != 401 {
			t.Errorf("%s host-token read=%d, want 401", name, w.Code)
		}
	}
}

func TestUploadOwnership(t *testing.T) {
	srv, hub, st, cfg, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	for _, id := range []string{"alice", "bob"} {
		_ = st.CreateParticipant(&store.Participant{ID: id, SessionID: "room", Status: "admitted", JoinedAt: time.Now()})
	}
	_ = st.CreateSource(&store.Source{ID: "alice-source", ParticipantID: "alice", Kind: "camera"})
	_ = st.CreateTake(&store.Take{ID: "take", SessionID: "room", Number: 1, WallClockStart: time.Now()})
	_ = st.CreateTrack(&store.Track{ID: "track", TakeID: "take", SourceID: "alice-source", Kind: "video", Mime: "video/webm", Status: "recording"})
	for _, tc := range []struct{ who, room, path string }{
		{"bob", "room", "/api/v1/takes/take/tracks/track/status"}, {"alice", "other", "/api/v1/takes/take/tracks/track/status"}, {"alice", "room", "/api/v1/takes/wrong-take/tracks/track/status"},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+signaling.MintUploadToken(tc.who, tc.room, time.Hour, cfg.SecretKey))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != 403 && w.Code != 404 {
			t.Errorf("%+v status=%d, want denied", tc, w.Code)
		}
	}
	_ = st.UpdateParticipantStatus("alice", "denied")
	req := httptest.NewRequest("GET", "/api/v1/takes/take/tracks/track/status", nil)
	req.Header.Set("Authorization", "Bearer "+signaling.MintUploadToken("alice", "room", time.Hour, cfg.SecretKey))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Errorf("revoked upload status=%d", w.Code)
	}
}

func TestParticipantRegistersOwnTrack(t *testing.T) {
	srv, hub, st, cfg, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	_ = st.CreateParticipant(&store.Participant{ID: "alice", SessionID: "room", Status: "admitted", JoinedAt: time.Now()})
	_ = st.CreateTake(&store.Take{ID: "take", SessionID: "room", Number: 1, WallClockStart: time.Now(), Status: "recording"})
	req := httptest.NewRequest("POST", "/api/v1/takes/take/tracks", bytes.NewBufferString(`{"kind":"video","mime":"video/webm;codecs=vp9,opus","label":"Camera"}`))
	req.Header.Set("Authorization", "Bearer "+signaling.MintUploadToken("alice", "room", time.Hour, cfg.SecretKey))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("register=%d: %s", w.Code, w.Body)
	}
	tracks, err := st.ListTracksByTake("take")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("no persisted track: %v %v", tracks, err)
	}
	src, err := st.GetSource(tracks[0].SourceID)
	if err != nil || src.ParticipantID != "alice" {
		t.Fatalf("wrong owner: %v %v", src, err)
	}
	req = httptest.NewRequest("PUT", "/api/v1/takes/take/tracks/"+tracks[0].ID+"/chunks/0", bytes.NewBufferString("chunk"))
	req.Header.Set("Authorization", "Bearer "+signaling.MintUploadToken("alice", "room", time.Hour, cfg.SecretKey))
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("registered upload=%d %s", w.Code, w.Body)
	}
}

func TestInviteRejectsLocalhostAndDoesNotRotateOnFailure(t *testing.T) {
	srv, hub, st, cfg, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()
	cfg.PublicURL = "http://localhost:7443"
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now(), InviteTokenHash: "original"})
	req := httptest.NewRequest("POST", "http://localhost:7443/api/v1/sessions/room/invite", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Authorization", "Bearer "+signaling.MintHostToken("room", time.Hour, cfg.SecretKey))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 422 {
		t.Errorf("localhost invite=%d, want422", w.Code)
	}
	sess, _ := st.GetSession("room")
	if sess.InviteTokenHash != "original" {
		t.Error("failed generation revoked existing invite")
	}
}
