package httpserver

import (
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadRequiresLocalHostCapability(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateSession(&store.Session{ID: "s", Title: "Synthetic", CreatedAt: time.Now(), PresetJSON: "{}"})
	st.CreateTake(&store.Take{ID: "t", SessionID: "s", Number: 1, Status: "complete", WallClockStart: time.Now()})
	cfg := &config.Config{SecretKey: []byte("synthetic-download-signing-key-12345")}
	s := &Server{Store: st, Config: cfg}
	token := signaling.MintHostToken("s", time.Hour, cfg.SecretKey)
	for _, tc := range []struct {
		name, token, forwarded string
		want                   int
	}{
		{"missing capability", "", "", 401}, {"guest capability", "guest", "", 401}, {"Serve cannot manage", token, "100.64.0.1", 403}, {"local host authorized", token, "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://localhost:7443/api/v1/takes/t/download", nil)
			r.RemoteAddr = "127.0.0.1:5000"
			r.Header.Set("Authorization", "Bearer "+tc.token)
			r.Header.Set("X-Forwarded-For", tc.forwarded)
			w := httptest.NewRecorder()
			s.handleTakeDownload(w, r, "t")
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
		})
	}
}
