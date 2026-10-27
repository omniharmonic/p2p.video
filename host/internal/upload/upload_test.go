package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func TestComputeContiguousRanges(t *testing.T) {
	tests := []struct {
		input    []int
		expected [][2]int
	}{
		{[]int{}, [][2]int{}},
		{[]int{0}, [][2]int{{0, 0}}},
		{[]int{0, 1, 2, 3}, [][2]int{{0, 3}}},
		{[]int{0, 1, 3, 4, 5, 8}, [][2]int{{0, 1}, {3, 5}, {8, 8}}},
		{[]int{5, 2, 0, 1, 4, 3}, [][2]int{{0, 5}}}, // unordered
		{[]int{0, 0, 1, 2, 2}, [][2]int{{0, 2}}},    // duplicates
	}

	for _, tt := range tests {
		got := ComputeContiguousRanges(tt.input)
		if !reflect.DeepEqual(got, tt.expected) {
			t.Errorf("input %v: got %v, want %v", tt.input, got, tt.expected)
		}
	}
}

func TestUploadFlow(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "db.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		DataDir:        filepath.Join(tempDir, "data"),
		SecretKey:      []byte("test-salt-secret-upload-key"),
		MaxChunkSizeMB: 10,
	}

	handler := &Handler{
		Store:  st,
		Config: cfg,
	}

	// Setup db state
	sess := &store.Take{
		ID:             "take_test_01",
		SessionID:      "sess_test",
		Number:         1,
		StartAtHostUs:  1000,
		WallClockStart: time.Now().UTC(),
		Status:         "recording",
	}
	_ = st.CreateSession(&store.Session{ID: "sess_test", Title: "T", CreatedAt: time.Now().UTC()})
	_ = st.CreateParticipant(&store.Participant{ID: "p_1", SessionID: "sess_test", Name: "N", Role: "guest", Status: "admitted", JoinedAt: time.Now().UTC()})
	_ = st.CreateSource(&store.Source{ID: "src_1", ParticipantID: "p_1", Kind: "camera", Label: "C"})
	_ = st.CreateTake(sess)
	_ = st.CreateTrack(&store.Track{
		ID:       "trk_video",
		TakeID:   "take_test_01",
		SourceID: "src_1",
		Kind:     "video",
		Mime:     "video/webm",
		Status:   "recording",
	})

	token := signaling.MintUploadToken("p_1", "sess_test", 1*time.Hour, cfg.SecretKey)

	// 1. Upload Chunk 0
	chunk0Data := []byte("CHUNK_DATA_ZERO_00000")
	h0 := sha256.Sum256(chunk0Data)
	h0Hex := hex.EncodeToString(h0[:])

	req0 := httptest.NewRequest(http.MethodPut, "/api/v1/takes/take_test_01/tracks/trk_video/chunks/0", bytes.NewReader(chunk0Data))
	req0.Header.Set("Authorization", "Bearer "+token)
	req0.Header.Set("X-Chunk-SHA256", h0Hex)
	rec0 := httptest.NewRecorder()
	handler.ServeHTTP(rec0, req0)

	if rec0.Code != http.StatusCreated {
		t.Fatalf("Chunk 0 failed: code %d, body: %s", rec0.Code, rec0.Body.String())
	}

	// 2. Upload Chunk 0 again (idempotent resume test)
	req0Retry := httptest.NewRequest(http.MethodPut, "/api/v1/takes/take_test_01/tracks/trk_video/chunks/0", bytes.NewReader(chunk0Data))
	req0Retry.Header.Set("Authorization", "Bearer "+token)
	req0Retry.Header.Set("X-Chunk-SHA256", h0Hex)
	rec0Retry := httptest.NewRecorder()
	handler.ServeHTTP(rec0Retry, req0Retry)

	if rec0Retry.Code != http.StatusOK {
		t.Fatalf("Chunk 0 retry failed: code %d, body: %s", rec0Retry.Code, rec0Retry.Body.String())
	}

	// 3. Upload Chunk 1
	chunk1Data := []byte("CHUNK_DATA_ONE_11111")
	h1 := sha256.Sum256(chunk1Data)
	h1Hex := hex.EncodeToString(h1[:])

	req1 := httptest.NewRequest(http.MethodPut, "/api/v1/takes/take_test_01/tracks/trk_video/chunks/1", bytes.NewReader(chunk1Data))
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.Header.Set("X-Chunk-SHA256", h1Hex)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusCreated {
		t.Fatalf("Chunk 1 failed: code %d, body: %s", rec1.Code, rec1.Body.String())
	}

	// 4. Check Status
	reqStatus := httptest.NewRequest(http.MethodGet, "/api/v1/takes/take_test_01/tracks/trk_video/status", nil)
	reqStatus.Header.Set("Authorization", "Bearer "+token)
	recStatus := httptest.NewRecorder()
	handler.ServeHTTP(recStatus, reqStatus)

	if recStatus.Code != http.StatusOK {
		t.Fatalf("Status failed: code %d", recStatus.Code)
	}
	var statResp StatusResponse
	_ = json.Unmarshal(recStatus.Body.Bytes(), &statResp)
	expectedRange := [][2]int{{0, 1}}
	if !reflect.DeepEqual(statResp.Have, expectedRange) {
		t.Fatalf("Status ranges mismatch: got %v, want %v", statResp.Have, expectedRange)
	}

	// 5. Complete Assembly
	compBody, _ := json.Marshal(CompleteRequest{
		LastSeq:    1,
		TotalBytes: int64(len(chunk0Data) + len(chunk1Data)),
		Sidecar: map[string]interface{}{
			"sampleRate": 48000,
		},
	})
	reqComp := httptest.NewRequest(http.MethodPost, "/api/v1/takes/take_test_01/tracks/trk_video/complete", bytes.NewReader(compBody))
	reqComp.Header.Set("Authorization", "Bearer "+token)
	recComp := httptest.NewRecorder()
	handler.ServeHTTP(recComp, reqComp)

	if recComp.Code != http.StatusOK {
		t.Fatalf("Complete failed: code %d, body: %s", recComp.Code, recComp.Body.String())
	}

	// Verify assembled file on disk
	assembledPath := filepath.Join(cfg.DataDir, "sessions", "sess_test", "takes", "take_test_01", "raw", "trk_video.webm")
	assembledBytes, err := os.ReadFile(assembledPath)
	if err != nil {
		t.Fatalf("Failed to read assembled file: %v", err)
	}
	expectedTotal := string(chunk0Data) + string(chunk1Data)
	if string(assembledBytes) != expectedTotal {
		t.Fatalf("Assembled content mismatch: got %q, want %q", string(assembledBytes), expectedTotal)
	}
}

// A stale finalization must not silently truncate a track that already has more fragments.
func TestAssemblyRejectsTruncatedManifest(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := &config.Config{DataDir: dir, SecretKey: []byte("test-secret"), MaxChunkSizeMB: 1}
	h := &Handler{Store: st, Config: cfg}
	_ = st.CreateSession(&store.Session{ID: "s", CreatedAt: time.Now()})
	_ = st.CreateParticipant(&store.Participant{ID: "p", SessionID: "s", Status: "admitted", JoinedAt: time.Now()})
	_ = st.CreateSource(&store.Source{ID: "src", ParticipantID: "p"})
	_ = st.CreateTake(&store.Take{ID: "take", SessionID: "s", Number: 1, WallClockStart: time.Now()})
	_ = st.CreateTrack(&store.Track{ID: "track", TakeID: "take", SourceID: "src", Mime: "video/webm", Status: "recording"})
	token := signaling.MintUploadToken("p", "s", time.Hour, cfg.SecretKey)
	for _, seq := range []string{"0", "1"} {
		req := httptest.NewRequest("PUT", "/api/v1/takes/take/tracks/track/chunks/"+seq, bytes.NewBufferString("fragment"))
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body)
		}
	}
	req := httptest.NewRequest("POST", "/api/v1/takes/take/tracks/track/complete", bytes.NewBufferString(`{"lastSeq":0,"totalBytes":8}`))
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatalf("truncation accepted: %d %s", w.Code, w.Body)
	}
	track, _ := st.GetTrack("track")
	if track.Status == "assembled" {
		t.Fatal("truncated track marked assembled")
	}
}
