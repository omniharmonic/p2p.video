package httpserver

import (
	"bytes"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// A screen share started mid-take registers its own late track as kind
// "screen"; the source type is validated and the take must still be recording.
func TestLateScreenTrackRegistration(t *testing.T) {
	srv, hub, st, cfg, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()
	_ = st.CreateSession(&store.Session{ID: "room", CreatedAt: time.Now()})
	_ = st.CreateParticipant(&store.Participant{ID: "alice", SessionID: "room", Status: "admitted", JoinedAt: time.Now()})
	_ = st.CreateTake(&store.Take{ID: "take", SessionID: "room", Number: 1, WallClockStart: time.Now(), Status: "recording"})
	register := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/takes/take/tracks", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+signaling.MintUploadToken("alice", "room", time.Hour, cfg.SecretKey))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}
	if w := register(`{"kind":"video","mime":"video/webm;codecs=vp9,opus","label":"Screen","source":"screen"}`); w.Code != 201 {
		t.Fatalf("screen register=%d: %s", w.Code, w.Body)
	}
	tracks, err := st.ListTracksByTake("take")
	if err != nil || len(tracks) != 1 {
		t.Fatalf("screen track not persisted: %v %v", tracks, err)
	}
	src, err := st.GetSource(tracks[0].SourceID)
	if err != nil || src.Kind != "screen" || src.ParticipantID != "alice" {
		t.Fatalf("screen source = %+v %v", src, err)
	}
	for _, body := range []string{
		`{"kind":"audio","mime":"audio/webm","label":"x","source":"screen"}`,
		`{"kind":"video","mime":"video/webm","label":"x","source":"window"}`,
	} {
		if w := register(body); w.Code != 400 {
			t.Errorf("%s accepted with %d", body, w.Code)
		}
	}
	tk, _ := st.GetTake("take")
	tk.Status = "stopped"
	_ = st.UpdateTake(tk)
	if w := register(`{"kind":"video","mime":"video/webm","label":"Screen","source":"screen"}`); w.Code != 409 {
		t.Errorf("screen registered after take stopped: %d", w.Code)
	}
}
