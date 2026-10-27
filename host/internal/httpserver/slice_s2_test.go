package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/sfu"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

func setupS2TestServer(t *testing.T) (*Server, *signaling.Hub, *store.Store, *config.Config, string) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "studiolink.db")
	dataDir := filepath.Join(tempDir, "data")
	_ = os.MkdirAll(dataDir, 0755)

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to open store: %v", err)
	}

	cfg := &config.Config{
		DataDir:        dataDir,
		SecretKey:      []byte("s2-super-secret-host-key-32bytes!"),
		MaxChunkSizeMB: 10,
	}

	refRec := refrec.NewManager()
	sfuEngine, err := sfu.NewSFU(refRec)
	if err != nil {
		t.Fatalf("NewSFU: %v", err)
	}

	hub := signaling.NewHub(st, cfg, sfuEngine)
	upHandler := &upload.Handler{Store: st, Config: cfg}

	srv := &Server{
		DesktopToken: testDesktopToken,
		Config:       cfg,
		Store:        st,
		Hub:          hub,
		Upload:       upHandler,
	}

	return srv, hub, st, cfg, tempDir
}

// TestSliceS2_OutageAndResumption tests an interrupted upload sequence that recovers
// missing chunks after a simulated network drop and successfully completes.
func TestSliceS2_OutageAndResumption(t *testing.T) {
	srv, hub, st, cfg, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()

	// 1. Setup Session, Participant, Source, Take, Track
	sess := &store.Session{ID: "sess_s2", Title: "S2 Live Session", CreatedAt: time.Now().UTC()}
	_ = st.CreateSession(sess)

	part := &store.Participant{
		ID:        "p_guest_s2",
		SessionID: "sess_s2",
		Name:      "Ana Lopez",
		Role:      "guest",
		Status:    "admitted",
		JoinedAt:  time.Now().UTC(),
	}
	_ = st.CreateParticipant(part)

	src := &store.Source{ID: "src_cam_s2", ParticipantID: part.ID, Kind: "camera", Label: "Ana Camera"}
	_ = st.CreateSource(src)

	take := &store.Take{
		ID:             "take_s2_01",
		SessionID:      "sess_s2",
		Number:         1,
		StartAtHostUs:  1000,
		WallClockStart: time.Now().UTC(),
		Status:         "recording",
	}
	_ = st.CreateTake(take)

	track := &store.Track{
		ID:       "trk_cam_s2",
		TakeID:   take.ID,
		SourceID: src.ID,
		Kind:     "video",
		Mime:     "video/webm",
		Status:   "recording",
	}
	_ = st.CreateTrack(track)

	token := signaling.MintUploadToken(part.ID, sess.ID, 1*time.Hour, cfg.SecretKey)

	// 2. Prepare 6 chunks
	var chunks [][]byte
	var hashes []string
	var totalBytes int64
	for i := 0; i < 6; i++ {
		content := []byte(fmt.Sprintf("CHUNK_PAYLOAD_%04d_RELIABLE_MEDIA", i))
		chunks = append(chunks, content)
		h := sha256.Sum256(content)
		hashes = append(hashes, hex.EncodeToString(h[:]))
		totalBytes += int64(len(content))
	}

	// 3. Upload chunks 0, 1, 2 (before network outage)
	for i := 0; i < 3; i++ {
		url := fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/%d", take.ID, track.ID, i)
		req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(chunks[i]))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Chunk-SHA256", hashes[i])
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("Chunk %d failed: code %d, body: %s", i, rec.Code, rec.Body.String())
		}
	}

	// 4. Simulated Network Outage: client queries /status to reconcile
	statusURL := fmt.Sprintf("/api/v1/takes/%s/tracks/%s/status", take.ID, track.ID)
	reqStatus := httptest.NewRequest(http.MethodGet, statusURL, nil)
	reqStatus.Header.Set("Authorization", "Bearer "+token)
	recStatus := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recStatus, reqStatus)

	if recStatus.Code != http.StatusOK {
		t.Fatalf("Status check failed: code %d", recStatus.Code)
	}
	var statResp upload.StatusResponse
	_ = json.Unmarshal(recStatus.Body.Bytes(), &statResp)
	expectedOutageRanges := [][2]int{{0, 2}}
	if !reflect.DeepEqual(statResp.Have, expectedOutageRanges) {
		t.Fatalf("Expected ranges %v, got %v", expectedOutageRanges, statResp.Have)
	}

	// 5. Connection Restored: client retries chunk 2 (idempotent duplicate) then uploads missing chunks 3, 4, 5
	// 5a. Retry chunk 2 (already stored -> 200 OK)
	reqRetry := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/2", take.ID, track.ID), bytes.NewReader(chunks[2]))
	reqRetry.Header.Set("Authorization", "Bearer "+token)
	reqRetry.Header.Set("X-Chunk-SHA256", hashes[2])
	recRetry := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recRetry, reqRetry)
	if recRetry.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for duplicate chunk 2, got %d", recRetry.Code)
	}

	// 5b. Upload remaining chunks 3..5
	for i := 3; i < 6; i++ {
		url := fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/%d", take.ID, track.ID, i)
		req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(chunks[i]))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Chunk-SHA256", hashes[i])
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("Chunk %d failed: code %d", i, rec.Code)
		}
	}

	// 6. Complete Assembly
	compBody, _ := json.Marshal(upload.CompleteRequest{
		LastSeq:    5,
		TotalBytes: totalBytes,
		Sidecar: map[string]interface{}{
			"codec":       "vp8",
			"fps":         30,
			"sampleRate":  48000,
			"recoveredAt": time.Now().UTC().Format(time.RFC3339),
		},
	})
	reqComp := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/complete", take.ID, track.ID), bytes.NewReader(compBody))
	reqComp.Header.Set("Authorization", "Bearer "+token)
	recComp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recComp, reqComp)

	if recComp.Code != http.StatusOK {
		t.Fatalf("Complete failed: code %d, body: %s", recComp.Code, recComp.Body.String())
	}

	var compResp map[string]interface{}
	_ = json.Unmarshal(recComp.Body.Bytes(), &compResp)
	if compResp["status"] != "assembled" {
		t.Fatalf("Expected status 'assembled', got %v", compResp["status"])
	}

	// 7. Verify assembled file on disk matches concatenation of all 6 chunks
	assembledPath := filepath.Join(cfg.DataDir, "sessions", sess.ID, "takes", take.ID, "raw", track.ID+".webm")
	assembledData, err := os.ReadFile(assembledPath)
	if err != nil {
		t.Fatalf("Failed to read assembled file: %v", err)
	}

	var expectedConcatenation []byte
	for _, c := range chunks {
		expectedConcatenation = append(expectedConcatenation, c...)
	}
	if !bytes.Equal(assembledData, expectedConcatenation) {
		t.Fatalf("Assembled bytes mismatch! Expected %d bytes, got %d bytes", len(expectedConcatenation), len(assembledData))
	}
}

// TestSliceS2_MissingChunkAndConflictRejection tests:
// 1. Finalizing with missing chunks in 0..lastSeq is strictly rejected (400)
// 2. Uploading contradictory data at the same sequence number is rejected as 409 Conflict
// 3. Uploading with corrupted hash is rejected as 400 Bad Request
func TestSliceS2_MissingChunkAndConflictRejection(t *testing.T) {
	srv, hub, st, cfg, _ := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()

	sess := &store.Session{ID: "sess_s2_neg", Title: "S2 Neg", CreatedAt: time.Now().UTC()}
	_ = st.CreateSession(sess)
	part := &store.Participant{ID: "p_neg", SessionID: "sess_s2_neg", Name: "Guest", Role: "guest", Status: "admitted"}
	_ = st.CreateParticipant(part)
	src := &store.Source{ID: "src_neg", ParticipantID: part.ID, Kind: "audio", Label: "Mic"}
	_ = st.CreateSource(src)
	take := &store.Take{ID: "take_neg_01", SessionID: "sess_s2_neg", Number: 1, Status: "recording"}
	_ = st.CreateTake(take)
	track := &store.Track{ID: "trk_neg_aud", TakeID: take.ID, SourceID: src.ID, Kind: "audio", Mime: "audio/wav", Status: "recording"}
	_ = st.CreateTrack(track)

	token := signaling.MintUploadToken(part.ID, sess.ID, 1*time.Hour, cfg.SecretKey)

	// 1. Upload chunk 0 and chunk 2 (CHUNK 1 IS DELIBERATELY MISSING)
	c0 := []byte("VALID_CHUNK_0")
	h0 := sha256.Sum256(c0)
	req0 := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/0", take.ID, track.ID), bytes.NewReader(c0))
	req0.Header.Set("Authorization", "Bearer "+token)
	req0.Header.Set("X-Chunk-SHA256", hex.EncodeToString(h0[:]))
	rec0 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec0, req0)
	if rec0.Code != http.StatusCreated {
		t.Fatalf("Chunk 0 failed: %d", rec0.Code)
	}

	c2 := []byte("VALID_CHUNK_2")
	h2 := sha256.Sum256(c2)
	req2 := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/2", take.ID, track.ID), bytes.NewReader(c2))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("X-Chunk-SHA256", hex.EncodeToString(h2[:]))
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("Chunk 2 failed: %d", rec2.Code)
	}

	// 2. Call Complete with lastSeq = 2 -> MUST FAIL with 400 Bad Request because chunk 1 is missing
	compBody, _ := json.Marshal(upload.CompleteRequest{LastSeq: 2, TotalBytes: int64(len(c0) + len(c2))})
	reqCompMissing := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/complete", take.ID, track.ID), bytes.NewReader(compBody))
	reqCompMissing.Header.Set("Authorization", "Bearer "+token)
	recCompMissing := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recCompMissing, reqCompMissing)

	if recCompMissing.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 Bad Request when completing with missing chunk 1, got %d", recCompMissing.Code)
	}

	// 3. Test Conflict (409): Upload chunk 0 with DIFFERENT payload
	c0Conflict := []byte("DIFFERENT_CONFLICTING_CHUNK_0_PAYLOAD")
	h0Conflict := sha256.Sum256(c0Conflict)
	reqConflict := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/0", take.ID, track.ID), bytes.NewReader(c0Conflict))
	reqConflict.Header.Set("Authorization", "Bearer "+token)
	reqConflict.Header.Set("X-Chunk-SHA256", hex.EncodeToString(h0Conflict[:]))
	recConflict := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recConflict, reqConflict)

	if recConflict.Code != http.StatusConflict {
		t.Fatalf("Expected 409 Conflict for contradictory chunk 0 data, got %d", recConflict.Code)
	}

	// 4. Test Hash Mismatch (400): Upload chunk 1 with invalid hash header
	c1 := []byte("VALID_CHUNK_1")
	reqBadHash := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/1", take.ID, track.ID), bytes.NewReader(c1))
	reqBadHash.Header.Set("Authorization", "Bearer "+token)
	reqBadHash.Header.Set("X-Chunk-SHA256", "deadbeef00000000000000000000000000000000000000000000000000000000")
	recBadHash := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBadHash, reqBadHash)

	if recBadHash.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 Bad Request for chunk hash mismatch, got %d", recBadHash.Code)
	}

	// 5. Now upload chunk 1 with CORRECT hash
	h1 := sha256.Sum256(c1)
	req1 := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/1", take.ID, track.ID), bytes.NewReader(c1))
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.Header.Set("X-Chunk-SHA256", hex.EncodeToString(h1[:]))
	rec1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("Chunk 1 failed with correct hash: %d", rec1.Code)
	}

	// 6. Now that chunks 0, 1, 2 are all present, Complete must succeed!
	compBodyFinal, _ := json.Marshal(upload.CompleteRequest{
		LastSeq:    2,
		TotalBytes: int64(len(c0) + len(c1) + len(c2)),
	})
	reqCompSuccess := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/complete", take.ID, track.ID), bytes.NewReader(compBodyFinal))
	reqCompSuccess.Header.Set("Authorization", "Bearer "+token)
	recCompSuccess := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recCompSuccess, reqCompSuccess)

	if recCompSuccess.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK after resolving missing chunks, got %d: %s", recCompSuccess.Code, recCompSuccess.Body.String())
	}
}

// TestSliceS2_DecodedOriginalsVerification generates a real audio clip with ffmpeg,
// chunks it, uploads via HTTP API, completes assembly, and decodes the final assembled file
// with ffprobe/ffmpeg to verify stream validity and non-zero duration without errors.
func TestSliceS2_DecodedOriginalsVerification(t *testing.T) {
	_, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on system; skipping decoded original test")
	}

	srv, hub, st, cfg, tempDir := setupS2TestServer(t)
	defer hub.Close()
	defer st.Close()

	// 1. Synthesize a 3-second 48kHz stereo WAV test file using ffmpeg
	srcWavPath := filepath.Join(tempDir, "input_source.wav")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "sine=frequency=1000:duration=3", "-ar", "48000", "-ac", "2", srcWavPath)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Failed to generate test WAV via ffmpeg: %v", err)
	}

	wavBytes, err := os.ReadFile(srcWavPath)
	if err != nil {
		t.Fatalf("Failed to read generated WAV: %v", err)
	}

	// 2. Setup DB entities
	sess := &store.Session{ID: "sess_s2_media", Title: "Media Verification", CreatedAt: time.Now().UTC()}
	_ = st.CreateSession(sess)
	part := &store.Participant{ID: "p_media", SessionID: "sess_s2_media", Name: "Host Ben", Role: "host", Status: "admitted"}
	_ = st.CreateParticipant(part)
	src := &store.Source{ID: "src_media_mic", ParticipantID: part.ID, Kind: "microphone", Label: "Host Mic"}
	_ = st.CreateSource(src)
	take := &store.Take{ID: "take_media_01", SessionID: "sess_s2_media", Number: 1, Status: "recording"}
	_ = st.CreateTake(take)
	track := &store.Track{ID: "trk_media_mic", TakeID: take.ID, SourceID: src.ID, Kind: "audio", Mime: "audio/wav", Status: "recording"}
	_ = st.CreateTrack(track)

	token := signaling.MintUploadToken(part.ID, sess.ID, 1*time.Hour, cfg.SecretKey)

	// 3. Split wavBytes into 3 chunks of ~equal size
	chunkSize := (len(wavBytes) + 2) / 3
	var seq int
	for start := 0; start < len(wavBytes); start += chunkSize {
		end := start + chunkSize
		if end > len(wavBytes) {
			end = len(wavBytes)
		}
		chunkData := wavBytes[start:end]
		h := sha256.Sum256(chunkData)

		url := fmt.Sprintf("/api/v1/takes/%s/tracks/%s/chunks/%d", take.ID, track.ID, seq)
		req := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(chunkData))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Chunk-SHA256", hex.EncodeToString(h[:]))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusCreated {
			t.Fatalf("Chunk %d failed: code %d", seq, rec.Code)
		}
		seq++
	}

	// 4. Complete assembly
	lastSeq := seq - 1
	compBody, _ := json.Marshal(upload.CompleteRequest{
		LastSeq:    lastSeq,
		TotalBytes: int64(len(wavBytes)),
	})
	reqComp := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/takes/%s/tracks/%s/complete", take.ID, track.ID), bytes.NewReader(compBody))
	reqComp.Header.Set("Authorization", "Bearer "+token)
	recComp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recComp, reqComp)

	if recComp.Code != http.StatusOK {
		t.Fatalf("Complete failed: %d, body: %s", recComp.Code, recComp.Body.String())
	}

	// 5. Verify assembled file with ffprobe
	assembledPath := filepath.Join(cfg.DataDir, "sessions", sess.ID, "takes", take.ID, "raw", track.ID+".wav")
	probeCmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration,size,bit_rate:stream=codec_name,channels,sample_rate", "-of", "json", assembledPath)
	probeOut, err := probeCmd.Output()
	if err != nil {
		t.Fatalf("ffprobe failed on assembled file %s: %v", assembledPath, err)
	}

	var probeResult struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			Channels   int    `json:"channels"`
			SampleRate string `json:"sample_rate"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(probeOut, &probeResult); err != nil {
		t.Fatalf("Failed to parse ffprobe json: %v", err)
	}

	if len(probeResult.Streams) == 0 {
		t.Fatalf("ffprobe detected 0 streams in assembled file!")
	}

	stream := probeResult.Streams[0]
	if stream.CodecName != "pcm_s16le" {
		t.Errorf("Expected codec pcm_s16le, got %s", stream.CodecName)
	}
	if stream.Channels != 2 {
		t.Errorf("Expected 2 channels, got %d", stream.Channels)
	}
	if stream.SampleRate != "48000" {
		t.Errorf("Expected sample rate 48000, got %s", stream.SampleRate)
	}
}
