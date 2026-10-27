package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/sfu"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

// setupStressServer configures a complete p2p.video test environment
func setupStressServer(t *testing.T) (*Server, *store.Store, *config.Config, *httptest.Server) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "studio_stress_data")
	_ = os.MkdirAll(dataDir, 0755)

	dbPath := filepath.Join(dataDir, "stress_test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}

	cfg := &config.Config{
		DataDir:        dataDir,
		HTTPPort:       7443,
		SecretKey:      []byte("stress-test-salt-secret-32-byte!"),
		MaxChunkSizeMB: 10,
	}

	refRec := refrec.NewManager()
	sfuEngine, err := sfu.NewSFU(refRec)
	if err != nil {
		t.Fatalf("NewSFU: %v", err)
	}

	hub := signaling.NewHub(st, cfg, sfuEngine)
	ff, _ := ffmpeg.NewRunner()
	pipe := postproc.NewPipeline(st, cfg, ff)

	upHandler := &upload.Handler{
		Store:  st,
		Config: cfg,
	}

	srv := &Server{
		DesktopToken: testDesktopToken,
		Config:       cfg,
		Store:        st,
		Hub:          hub,
		Upload:       upHandler,
		Pipeline:     pipe,
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { hub.Close(); st.Close() })
	return srv, st, cfg, ts
}

// TestNetworkJitterAndRetryBackoff simulates erratic network conditions with latency and drop retries
func TestNetworkJitterAndRetryBackoff(t *testing.T) {
	_, st, cfg, ts := setupStressServer(t)
	defer ts.Close()

	sessID := "sess_jitter_01"
	_ = st.CreateSession(&store.Session{
		ID:              sessID,
		Title:           "Jitter Stress Session",
		CreatedAt:       time.Now().UTC(),
		PresetJSON:      "{}",
		InviteTokenHash: "test_hash",
		AutoAdmit:       true,
	})

	partID := "p_jitter_user"
	_ = st.CreateParticipant(&store.Participant{
		ID:        partID,
		SessionID: sessID,
		Name:      "Jittery Participant",
		Role:      "guest",
		Status:    "admitted",
		JoinedAt:  time.Now().UTC(),
	})

	srcID := "src_jitter_cam"
	_ = st.CreateSource(&store.Source{
		ID:            srcID,
		ParticipantID: partID,
		Kind:          "video",
		Label:         "Jitter Cam",
	})

	takeID := "take_jitter_01"
	_ = st.CreateTake(&store.Take{
		ID:             takeID,
		SessionID:      sessID,
		Number:         1,
		StartAtHostUs:  1000000,
		WallClockStart: time.Now().UTC(),
		Status:         "recording",
	})

	trackID := "trk_jitter_track"
	_ = st.CreateTrack(&store.Track{
		ID:       trackID,
		TakeID:   takeID,
		SourceID: srcID,
		Kind:     "video",
		Mime:     "video/webm",
		Status:   "recording",
	})

	uploadToken := signaling.MintUploadToken(partID, sessID, 1*time.Hour, cfg.SecretKey)
	client := &http.Client{Timeout: 10 * time.Second}

	// Upload 5 chunks under simulated jitter and transient retries
	numChunks := 5
	for seq := 0; seq < numChunks; seq++ {
		chunkPayload := []byte(fmt.Sprintf("CHUNK_JITTER_DATA_SEQ_%d_%d", seq, rand.Intn(10000)))
		h := sha256.Sum256(chunkPayload)
		hHex := hex.EncodeToString(h[:])

		putURL := fmt.Sprintf("%s/api/v1/takes/%s/tracks/%s/chunks/%d", ts.URL, takeID, trackID, seq)

		// Simulate exponential backoff retry loop
		var resp *http.Response
		var err error
		backoff := 20 * time.Millisecond
		maxRetries := 4

		for attempt := 0; attempt <= maxRetries; attempt++ {
			// Simulate jitter delay (10ms - 50ms)
			jitter := time.Duration(10+rand.Intn(40)) * time.Millisecond
			time.Sleep(jitter)

			req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(chunkPayload))
			req.Header.Set("Authorization", "Bearer "+uploadToken)
			req.Header.Set("X-Chunk-SHA256", hHex)

			resp, err = client.Do(req)
			if err == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated) {
				break
			}

			// If failed, sleep backoff
			time.Sleep(backoff)
			backoff *= 2
		}

		if err != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) {
			t.Fatalf("Chunk %d failed after retries: err=%v, code=%d", seq, err, resp.StatusCode)
		}
	}

	// Verify all chunks are recorded in SQLite
	seqs, err := st.GetStoredChunkSeqs(trackID)
	if err != nil || len(seqs) != numChunks {
		t.Fatalf("Expected %d chunks in database, got %d (err: %v)", numChunks, len(seqs), err)
	}
}

// TestAbruptSocketTerminationAndReconnection verifies that reconnecting cannot
// take over a previous socket's server-assigned participant identity.
func TestAbruptSocketTerminationAndReconnection(t *testing.T) {
	_, st, _, ts := setupStressServer(t)
	defer ts.Close()
	session := "sess_reconnect_01"
	token := "reconnect_token_secret"
	tokenHashBytes := sha256.Sum256([]byte(token))
	if err := st.CreateSession(&store.Session{ID: session, Title: "Socket Termination Test", CreatedAt: time.Now().UTC(), PresetJSON: "{}", InviteTokenHash: hex.EncodeToString(tokenHashBytes[:]), AutoAdmit: true}); err != nil {
		t.Fatal(err)
	}
	wsURL, _ := url.Parse(ts.URL)
	wsURL.Scheme = "ws"
	wsURL.Path = "/api/v1/ws"
	query := wsURL.Query()
	query.Set("session", session)
	query.Set("token", token)
	query.Set("participantId", "p_reconnect_guest")
	wsURL.RawQuery = query.Encode()
	connect := func() (*websocket.Conn, signaling.AdmissionPayload) {
		t.Helper()
		conn, _, err := websocket.DefaultDialer.Dial(wsURL.String(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteJSON(map[string]interface{}{"t": "hello", "participantId": "p_reconnect_guest", "payload": map[string]interface{}{"name": "Guest Bob", "role": "guest"}}); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			var envelope signaling.Envelope
			if err := conn.ReadJSON(&envelope); err != nil {
				conn.Close()
				t.Fatal(err)
			}
			if envelope.Type != signaling.TypeAdmission {
				continue
			}
			var admission signaling.AdmissionPayload
			if err := json.Unmarshal(envelope.Payload, &admission); err != nil {
				t.Fatal(err)
			}
			if admission.Status != "admitted" || admission.ParticipantID == "" || admission.ParticipantID == "p_reconnect_guest" {
				t.Fatalf("invalid server admission: %+v", admission)
			}
			return conn, admission
		}
	}
	first, firstAdmission := connect()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, secondAdmission := connect()
	defer second.Close()
	if firstAdmission.ParticipantID == secondAdmission.ParticipantID {
		t.Fatal("reconnection reused a caller-selected socket identity")
	}
	// Wait for the read-loop cleanup rather than assuming TCP close is synchronous.
	deadline := time.Now().Add(2 * time.Second)
	for {
		original, err := st.GetParticipant(firstAdmission.ParticipantID)
		if err != nil {
			t.Fatal(err)
		}
		if original.Status == "disconnected" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("old socket remains %s", original.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
	current, err := st.GetParticipant(secondAdmission.ParticipantID)
	if err != nil || current.Status != "admitted" {
		t.Fatalf("new connection lost admission: %+v %v", current, err)
	}
	participants, err := st.ListParticipants(session)
	if err != nil || len(participants) != 2 {
		t.Fatalf("expected separate socket records: %+v %v", participants, err)
	}
	for _, participant := range participants {
		if participant.ID == "p_reconnect_guest" {
			t.Fatal("caller-supplied identity persisted")
		}
	}
}

// TestChunkReuploadIdempotencyAndHashConflict verifies HTTP 200 on identical chunk and HTTP 409 on corrupt hash
func TestChunkReuploadIdempotencyAndHashConflict(t *testing.T) {
	_, st, cfg, ts := setupStressServer(t)
	defer ts.Close()

	sessID := "sess_idempotent_01"
	_ = st.CreateSession(&store.Session{
		ID:              sessID,
		Title:           "Idempotency Test",
		CreatedAt:       time.Now().UTC(),
		PresetJSON:      "{}",
		InviteTokenHash: "hash",
		AutoAdmit:       true,
	})

	partID := "p_idem_guest"
	_ = st.CreateParticipant(&store.Participant{
		ID:        partID,
		SessionID: sessID,
		Name:      "Idem Guest",
		Role:      "guest",
		Status:    "admitted",
		JoinedAt:  time.Now().UTC(),
	})

	_ = st.CreateSource(&store.Source{
		ID:            "src_cam",
		ParticipantID: partID,
		Kind:          "video",
		Label:         "Camera",
	})

	takeID := "take_idem_01"
	_ = st.CreateTake(&store.Take{
		ID:             takeID,
		SessionID:      sessID,
		Number:         1,
		Status:         "recording",
		WallClockStart: time.Now().UTC(),
	})

	trackID := "trk_idem_video"
	_ = st.CreateTrack(&store.Track{
		ID:       trackID,
		TakeID:   takeID,
		SourceID: "src_cam",
		Kind:     "video",
		Mime:     "video/webm",
		Status:   "recording",
	})

	uploadToken := signaling.MintUploadToken(partID, sessID, 1*time.Hour, cfg.SecretKey)
	client := &http.Client{}

	chunkData := []byte("GENUINE_VIDEO_CHUNK_SEQ_0_CONTENT_BYTES")
	h := sha256.Sum256(chunkData)
	hHex := hex.EncodeToString(h[:])

	putURL := fmt.Sprintf("%s/api/v1/takes/%s/tracks/%s/chunks/0", ts.URL, takeID, trackID)

	// 1. Initial upload: must succeed with 200 or 201
	req1, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(chunkData))
	req1.Header.Set("Authorization", "Bearer "+uploadToken)
	req1.Header.Set("X-Chunk-SHA256", hHex)
	resp1, err := client.Do(req1)
	if err != nil || (resp1.StatusCode != http.StatusOK && resp1.StatusCode != http.StatusCreated) {
		t.Fatalf("Initial chunk upload failed: %v, code: %d", err, resp1.StatusCode)
	}

	// 2. Re-upload IDENTICAL chunk (idempotency check): MUST return 200 OK
	req2, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(chunkData))
	req2.Header.Set("Authorization", "Bearer "+uploadToken)
	req2.Header.Set("X-Chunk-SHA256", hHex)
	resp2, err := client.Do(req2)
	if err != nil || resp2.StatusCode != http.StatusOK {
		t.Fatalf("Expected HTTP 200 on duplicate identical chunk, got: %v (code %d)", err, resp2.StatusCode)
	}

	// 3. Corrupt chunk re-upload (same seq, different payload or corrupt hash): MUST return 409 Conflict
	corruptData := []byte("CORRUPTED_TAMPERED_CHUNK_CONTENT_DIFFERENT_HASH")
	corruptH := sha256.Sum256(corruptData)
	corruptHex := hex.EncodeToString(corruptH[:])

	req3, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(corruptData))
	req3.Header.Set("Authorization", "Bearer "+uploadToken)
	req3.Header.Set("X-Chunk-SHA256", corruptHex)
	resp3, err := client.Do(req3)
	if err != nil {
		t.Fatalf("Failed to execute corrupt request: %v", err)
	}
	if resp3.StatusCode != http.StatusConflict {
		t.Fatalf("Expected HTTP 409 Conflict on corrupt chunk re-upload, got: %d", resp3.StatusCode)
	}

	// 4. Verify host /status returns contiguous range [0, 0]
	statusURL := fmt.Sprintf("%s/api/v1/takes/%s/tracks/%s/status", ts.URL, takeID, trackID)
	statusReq, _ := http.NewRequest(http.MethodGet, statusURL, nil)
	statusReq.Header.Set("Authorization", "Bearer "+uploadToken)
	statusResp, err := client.Do(statusReq)
	if err != nil || statusResp.StatusCode != http.StatusOK {
		t.Fatalf("GET status failed: %v", err)
	}
	var statusBody upload.StatusResponse
	_ = json.NewDecoder(statusResp.Body).Decode(&statusBody)
	if len(statusBody.Have) != 1 || statusBody.Have[0][0] != 0 || statusBody.Have[0][1] != 0 {
		t.Fatalf("Unexpected have ranges: %+v", statusBody.Have)
	}
}

// TestSettingsPersistenceAPI verifies PATCH /api/v1/sessions/{id}/settings and SQLite store updates
func TestSettingsPersistenceAPI(t *testing.T) {
	_, st, cfg, ts := setupStressServer(t)
	defer ts.Close()

	sessID := "sess_settings_test"
	_ = st.CreateSession(&store.Session{
		ID:              sessID,
		Title:           "Settings Persistence Session",
		CreatedAt:       time.Now().UTC(),
		PresetJSON:      "{}",
		InviteTokenHash: "hash123",
		AutoAdmit:       true,
	})

	client := &http.Client{}
	patchURL := fmt.Sprintf("%s/api/v1/sessions/%s/settings", ts.URL, sessID)

	payload := map[string]interface{}{
		"video_bitrate_bps": 25000000,
		"video_codec":       "vp9",
		"target_fps":        29.97,
		"audio_sample_rate": 96000,
		"audio_bit_depth":   24,
		"xcorr_threshold":   0.75,
		"xcorr_window_ms":   750,
		"engine":            "webcodecs",
	}
	pBytes, _ := json.Marshal(payload)

	unauthenticated, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(pBytes))
	denied, err := client.Do(unauthenticated)
	if err != nil {
		t.Fatal(err)
	}
	denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized settings mutation=%d", denied.StatusCode)
	}
	hostToken := signaling.MintHostToken(sessID, time.Hour, cfg.SecretKey)
	req, _ := http.NewRequest(http.MethodPatch, patchURL, bytes.NewReader(pBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+hostToken)
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH settings failed: %v (code %d)", err, resp.StatusCode)
	}

	var updatedSettings store.SessionSettings
	_ = json.NewDecoder(resp.Body).Decode(&updatedSettings)

	if updatedSettings.VideoBitrateBps != 25000000 || updatedSettings.VideoCodec != "vp9" || updatedSettings.TargetFPS != 29.97 {
		t.Fatalf("Settings not updated properly in response: %+v", updatedSettings)
	}

	// Verify persistence in SQLite store
	sess, err := st.GetSession(sessID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if !strings.Contains(sess.PresetJSON, "25000000") || !strings.Contains(sess.PresetJSON, "vp9") {
		t.Fatalf("PresetJSON in SQLite not updated: %s", sess.PresetJSON)
	}

	// Verify GET /api/v1/sessions/{id} returns settings object
	getURL := fmt.Sprintf("%s/api/v1/sessions/%s", ts.URL, sessID)
	getReq, _ := http.NewRequest(http.MethodGet, getURL, nil)
	getReq.Header.Set("Authorization", "Bearer "+hostToken)
	getResp, err := client.Do(getReq)
	if err != nil || getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET session failed: %v", err)
	}
	var getBody map[string]interface{}
	_ = json.NewDecoder(getResp.Body).Decode(&getBody)
	if getBody["settings"] == nil {
		t.Fatalf("Expected settings in session detail response, got nil")
	}
}

// TestAudioDriftAndAlignmentVerification checks raw acoustic lag separately
// from measured coarse clock alignment; reference samples have no host origin.
func TestAudioDriftAndAlignmentVerification(t *testing.T) {
	sampleRate := 48000
	numSamples := sampleRate * 1 // 1 second

	// Generate synthetic audio signal with a unique acoustic burst
	refSamples := make([]float64, numSamples)
	for i := 0; i < numSamples; i++ {
		tSec := float64(i) / float64(sampleRate)
		refSamples[i] = math.Sin(2*math.Pi*440*tSec) + 0.5*math.Sin(2*math.Pi*880*tSec)
	}
	// Add unique transient marker
	burstStart := sampleRate / 4
	for i := 0; i < 240; i++ {
		refSamples[burstStart+i] += 5.0
	}

	// Simulate delay of +480 samples (10.0 milliseconds)
	delaySamples := 480
	localSamples := make([]float64, numSamples)
	for i := delaySamples; i < numSamples; i++ {
		localSamples[i] = refSamples[i-delaySamples]
	}

	// Execute cross-correlation
	bestLag, peak := align.CrossCorrelate(refSamples, localSamples, sampleRate/4)
	if bestLag != delaySamples {
		t.Fatalf("Expected lag %d, got %d (peak %f)", delaySamples, bestLag, peak)
	}

	// Missing timing metadata must fail rather than invent zero or fine alignment.
	if result, err := align.AlignTrackWithReferenceConfig("", 1_000_000, localSamples, refSamples, sampleRate, 0.5, 500); err == nil || result != nil {
		t.Fatalf("missing sidecar invented timing: %+v %v", result, err)
	}
	sidecarPath := filepath.Join(t.TempDir(), "clock.sidecar.json")
	sidecar := align.Sidecar{TrackID: "track", Kind: "audio", ClockFit: align.ClockFitData{A: 1_000_000, B: 1.00004, RttP10Ms: 6, R2: 0.99}, Events: []align.SyncEvent{{Type: "first-sample", PerfTime: 20, MediaTimeUs: 0}}}
	data, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecarPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := align.AlignTrackWithReferenceConfig(sidecarPath, 1_000_000, localSamples, refSamples, sampleRate, 0.5, 500)
	if err != nil {
		t.Fatal(err)
	}
	if result.Method != "sidecar-coarse" || math.Abs(result.OffsetSec-0.0200008) > 1e-9 {
		t.Fatalf("unanchored acoustic samples replaced clock estimate: %+v", result)
	}
	if math.Abs(result.DriftPpm-40) > 1e-6 || math.Abs(result.ResidualMs-3) > 1e-9 {
		t.Fatalf("coarse clock metrics=%+v", result)
	}

}

// TestDoctorAndTailscaleEndpoints tests GET /api/v1/doctor and GET /api/v1/tailscale/status
func TestDoctorAndTailscaleEndpoints(t *testing.T) {
	_, _, _, ts := setupStressServer(t)
	defer ts.Close()

	// 1. /api/v1/doctor
	docResp, err := adminGet(ts.URL + "/api/v1/doctor")
	if err != nil || docResp.StatusCode != http.StatusOK {
		t.Fatalf("Doctor endpoint failed: %v", err)
	}
	var docBody map[string]interface{}
	_ = json.NewDecoder(docResp.Body).Decode(&docBody)
	if docBody["status"] != "ok" || docBody["ffmpeg"] == nil || docBody["tailscale"] == nil {
		t.Fatalf("Invalid doctor response: %+v", docBody)
	}

	// 2. /api/v1/tailscale/status
	tsResp, err := adminGet(ts.URL + "/api/v1/tailscale/status")
	if err != nil || tsResp.StatusCode != http.StatusOK {
		t.Fatalf("Tailscale status endpoint failed: %v", err)
	}
	var tsBody map[string]interface{}
	_ = json.NewDecoder(tsResp.Body).Decode(&tsBody)
	if tsBody["online"] == nil {
		t.Fatalf("Missing online status in tailscale response: %+v", tsBody)
	}
}
