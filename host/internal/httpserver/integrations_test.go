package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/integrations"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// The post-take actions answer only the session's host on the local
// dashboard; a guest invite token or another session's host token is refused.
func TestTakeIntegrationRoutesAreHostOnly(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{DataDir: root, SecretKey: []byte("integration-route-secret-0123456789")}
	for _, id := range []string{"s1", "s2"} {
		if err := st.CreateSession(&store.Session{ID: id, Title: "Pilot", CreatedAt: time.Now(), PresetJSON: "{}", InviteTokenHash: "hash-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateTake(&store.Take{ID: "take1", SessionID: "s1", Number: 1, Status: "complete", WallClockStart: time.Now()}); err != nil {
		t.Fatal(err)
	}
	pkg := filepath.Join(root, "exports", "Pilot_s1", "take_01_take1")
	if err := os.MkdirAll(filepath.Join(pkg, "audio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := export.WriteManifest(pkg, "s1", "Pilot", 1, time.Now(), 30, []export.ExportSource{{ID: "a", Participant: "Ana", Kind: "audio", FileRelPath: "audio/a.wav", DurationSec: 2}}); err != nil {
		t.Fatal(err)
	}
	pipe := postproc.NewPipeline(st, cfg, nil)
	svc := &integrations.Service{Store: st, Package: pipe, MinutesReason: "minutes-cli not found"}
	defer svc.Close()
	srv := &Server{Config: cfg, Store: st, Pipeline: pipe, Integrations: svc}
	host := signaling.MintHostToken("s1", time.Hour, cfg.SecretKey)
	other := signaling.MintHostToken("s2", time.Hour, cfg.SecretKey)

	do := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:7443"+path, nil)
		r.RemoteAddr = "127.0.0.1:50000"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w
	}
	base := "/api/v1/takes/take1"
	for _, tc := range []struct{ method, path string }{
		{"GET", base + "/integrations"}, {"POST", base + "/transcript"}, {"POST", base + "/parachute"}, {"POST", base + "/handoff/clip-editor"},
	} {
		for _, tok := range []string{"", other, "guest-invite-token"} {
			if w := do(tc.method, tc.path, tok); w.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %q = %d, want 401", tc.method, tc.path, tok, w.Code)
			}
		}
	}
	if w := do("POST", base+"/integrations", host); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method = %d", w.Code)
	}
	if w := do("GET", "/api/v1/takes/missing/integrations", host); w.Code != http.StatusNotFound {
		t.Errorf("missing take = %d", w.Code)
	}

	w := do("GET", base+"/integrations", host)
	var view integrations.View
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || !view.PackageReady || view.Capabilities.Minutes.Available || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("view %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", base+"/transcript", host); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "built-in transcription") {
		t.Fatalf("transcript without an engine = %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", base+"/parachute", host); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("parachute unconfigured = %d", w.Code)
	}
	if w := do("POST", base+"/handoff/clip-editor", host); w.Code != http.StatusOK {
		t.Fatalf("clip editor = %d %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(pkg, "handoff", "clip-editor.json")); err != nil {
		t.Fatal(err)
	}
	// Auto-transcribe is a host session setting.
	r := httptest.NewRequest("PATCH", "http://127.0.0.1:7443/api/v1/sessions/s1/settings", strings.NewReader(`{"auto_transcribe":true}`))
	r.RemoteAddr = "127.0.0.1:50000"
	r.Header.Set("Authorization", "Bearer "+host)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"auto_transcribe":true`) {
		t.Fatalf("settings %d %s", rec.Code, rec.Body.String())
	}
	if v := svc.View("take1"); !v.AutoTranscribe {
		t.Fatal("auto-transcribe not persisted")
	}
}
