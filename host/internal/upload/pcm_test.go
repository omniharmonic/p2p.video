package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestPCMDeferredHeaderAndPartialReceipt(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{DataDir: root, SecretKey: []byte("pcm-test-only"), MaxChunkSizeMB: 2}
	h := &Handler{Store: st, Config: cfg}
	for _, err := range []error{
		st.CreateSession(&store.Session{ID: "s", Title: "PCM", CreatedAt: time.Now()}),
		st.CreateParticipant(&store.Participant{ID: "p", SessionID: "s", Name: "Speaker", Status: "admitted", Role: "guest", JoinedAt: time.Now()}),
		st.CreateSource(&store.Source{ID: "src", ParticipantID: "p", Kind: "mic", Label: "PCM"}),
		st.CreateTake(&store.Take{ID: "take", SessionID: "s", Number: 1, Status: "stopped", WallClockStart: time.Now()}),
		st.CreateTrack(&store.Track{ID: "audio", SourceID: "src", TakeID: "take", Kind: "audio", Mime: "audio/wav", Status: "recording"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	token := signaling.MintUploadToken("p", "s", time.Hour, cfg.SecretKey)
	request := func(method, route string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/takes/take/tracks/audio/"+route, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		if method == http.MethodPut {
			digest := sha256.Sum256(body)
			req.Header.Set("X-Chunk-SHA256", hex.EncodeToString(digest[:]))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	samples := make([]byte, 16)
	if response := request(http.MethodPut, "chunks/1", samples); response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	manifest := map[string]any{"lastSeq": 1, "totalBytes": 74, "sidecar": map[string]any{"captureComplete": false, "pcm": map[string]any{"format": "float32le", "sampleRate": 48000, "channels": 1, "frames": 4}}}
	body, _ := json.Marshal(manifest)
	if response := request(http.MethodPost, "complete", body); response.Code != 400 {
		t.Fatalf("missing header accepted: %d %s", response.Code, response.Body.String())
	}
	tr, _ := st.GetTrack("audio")
	if tr.Status != "recording" {
		t.Fatalf("track finalized before header: %s", tr.Status)
	}
	if response := request(http.MethodPut, "chunks/0", make([]byte, 58)); response.Code != 201 {
		t.Fatal(response.Body.String())
	}
	if response := request(http.MethodPost, "complete", body); response.Code != 200 {
		t.Fatalf("partial receipt failed: %d %s", response.Code, response.Body.String())
	}
	tr, _ = st.GetTrack("audio")
	if tr.Status != "partial" {
		t.Fatalf("crash prefix must stay visibly incomplete, got %s", tr.Status)
	}
}
