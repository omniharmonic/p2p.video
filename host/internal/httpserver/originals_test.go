package httpserver

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Originals are host-only and downloadable as soon as every track is
// assembled; the take status reports readiness [SL-P0-9].
func TestOriginalsDownloadAndTakeStatus(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{DataDir: root, SecretKey: []byte("originals-route-secret-0123456789ab")}
	now := time.Now()
	for _, err := range []error{
		st.CreateSession(&store.Session{ID: "s1", Title: "Pilot", CreatedAt: now, PresetJSON: "{}", InviteTokenHash: "hash"}),
		st.CreateSession(&store.Session{ID: "s2", Title: "Other", CreatedAt: now, PresetJSON: "{}", InviteTokenHash: "hash2"}),
		st.CreateTake(&store.Take{ID: "take1", SessionID: "s1", Number: 1, StartAtHostUs: 1_000_000, Status: "stopped", WallClockStart: now}),
		st.CreateParticipant(&store.Participant{ID: "p", SessionID: "s1", Name: "Ana", Role: "host", Status: "admitted", JoinedAt: now}),
		st.CreateSource(&store.Source{ID: "cam", ParticipantID: "p", Kind: "camera", Label: "Camera"}),
		st.CreateTrack(&store.Track{ID: "video", TakeID: "take1", SourceID: "cam", Kind: "video", Mime: "video/mp4", Status: "uploading"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := filepath.Join(root, "sessions", "s1", "takes", "take1", "raw")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raw, "video.mp4"), []byte("media-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	side, _ := json.Marshal(align.Sidecar{TrackID: "video", ClockFit: align.ClockFitData{A: 1_500_000, B: 1, R2: 1, RttP10Ms: 1}, Events: []align.SyncEvent{{Type: "recorder-start", PerfTime: 0}}})
	if err := os.WriteFile(filepath.Join(raw, "video.sidecar.json"), side, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Config: cfg, Store: st, Pipeline: postproc.NewPipeline(st, cfg, nil)}
	host := signaling.MintHostToken("s1", time.Hour, cfg.SecretKey)
	other := signaling.MintHostToken("s2", time.Hour, cfg.SecretKey)
	do := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1:7443"+path, nil)
		r.RemoteAddr = "127.0.0.1:50000"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	ready := func() bool {
		var body struct {
			OriginalsReady bool `json:"originalsReady"`
		}
		w := do("/api/v1/takes/take1", host)
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("take status %d %s", w.Code, w.Body)
		}
		return body.OriginalsReady
	}
	for _, token := range []string{"", "guest", other} {
		if w := do("/api/v1/takes/take1/originals", token); w.Code != http.StatusUnauthorized {
			t.Errorf("token %q got %d", token, w.Code)
		}
	}
	if ready() {
		t.Fatal("originals ready while uploading")
	}
	if w := do("/api/v1/takes/take1/originals", host); w.Code != http.StatusConflict {
		t.Fatalf("unassembled originals: %d", w.Code)
	}
	tr, _ := st.GetTrack("video")
	tr.Status = "assembled"
	if err := st.UpdateTrack(tr); err != nil {
		t.Fatal(err)
	}
	if !ready() {
		t.Fatal("originals not ready after assembly")
	}
	w := do("/api/v1/takes/take1/originals?token="+host, "")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" || w.Header().Get("Content-Disposition") != `attachment; filename="p2p.video-take-01-originals.zip"` {
		t.Fatalf("download %d %v", w.Code, w.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["originals/Ana_Camera_video.mp4"] || !names["manifest.json"] || !names["offsets.json"] || !names["README.txt"] {
		t.Fatalf("entries %v", names)
	}

	// The editor-package codec is a validated host session setting.
	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "http://127.0.0.1:7443/api/v1/sessions/s1/settings", bytes.NewBufferString(body))
		r.RemoteAddr = "127.0.0.1:50000"
		r.Header.Set("Authorization", "Bearer "+host)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	if w := patch(`{"export_codec":"prores_4444"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid codec accepted: %d", w.Code)
	}
	if w := patch(`{"export_codec":"prores_proxy"}`); w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"export_codec":"prores_proxy"`)) {
		t.Fatalf("codec setting %d %s", w.Code, w.Body)
	}
}
