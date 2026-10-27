package httpserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/postproc"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/sfu"
	"github.com/omniharmonic/studiolink/host/internal/signaling"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/upload"
)

func TestStudioLinkEndToEndFlow(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "studio_data")
	_ = os.MkdirAll(dataDir, 0755)

	dbPath := filepath.Join(dataDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		DataDir:        dataDir,
		HTTPPort:       7443,
		SecretKey:      []byte("e2e-test-salt-secret-key-32b!"),
		MaxChunkSizeMB: 10,
	}

	refRec := refrec.NewManager()
	sfuEngine, err := sfu.NewSFU(refRec)
	if err != nil {
		t.Fatalf("NewSFU: %v", err)
	}

	hub := signaling.NewHub(st, cfg, sfuEngine)
	defer hub.Close()

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
	defer ts.Close()

	// 1. Check health
	resp, err := http.Get(ts.URL + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("Health check failed: %v", err)
	}

	// 2. Create Session
	createReq, _ := json.Marshal(map[string]interface{}{
		"title":     "Test Episode Multicam",
		"autoAdmit": true,
	})
	sessResp, err := adminPost(ts.URL+"/api/v1/sessions", string(createReq))
	if err != nil || sessResp.StatusCode != http.StatusOK {
		t.Fatalf("Create session failed: %v", err)
	}
	var sessData CreateSessionResponse
	_ = json.NewDecoder(sessResp.Body).Decode(&sessData)
	if sessData.ID == "" || sessData.Token == "" {
		t.Fatalf("Invalid session response: %+v", sessData)
	}

	// 3. Setup Participant and Source in DB
	partID := "p_guest_1"
	_ = st.CreateParticipant(&store.Participant{
		ID:        partID,
		SessionID: sessData.ID,
		Name:      "Guest Alice",
		Role:      "guest",
		Status:    "admitted",
		JoinedAt:  time.Now().UTC(),
	})
	srcID := "src_cam_alice"
	_ = st.CreateSource(&store.Source{
		ID:            srcID,
		ParticipantID: partID,
		Kind:          "video",
		Label:         "Sony A7IV",
	})

	// 4. Create Take
	takeID := fmt.Sprintf("take_%s_01", sessData.ID)
	tk := &store.Take{
		ID:             takeID,
		SessionID:      sessData.ID,
		Number:         1,
		StartAtHostUs:  1000000,
		WallClockStart: time.Now().UTC(),
		Status:         "recording",
	}
	if err := st.CreateTakeWithParticipants(tk, []string{partID}); err != nil {
		t.Fatal(err)
	}

	// 5. Create Track
	trackID := fmt.Sprintf("trk_%s_video", partID)
	_ = st.CreateTrack(&store.Track{
		ID:       trackID,
		TakeID:   takeID,
		SourceID: srcID,
		Kind:     "video",
		Mime:     "video/webm",
		Status:   "recording",
	})

	uploadToken := signaling.MintUploadToken(partID, sessData.ID, 1*time.Hour, cfg.SecretKey)

	// 6. Upload 3 progressive chunks using genuine WebM media generated with FFmpeg
	tempWebm := filepath.Join(tempDir, "source.webm")
	cmd := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "testsrc=duration=1:size=320x240:rate=30", "-c:v", "vp8", tempWebm)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Failed to generate test WebM: %v (output: %s)", err, string(out))
	}
	realWebmBytes, err := os.ReadFile(tempWebm)
	if err != nil {
		t.Fatalf("Failed to read test WebM: %v", err)
	}

	chunkSize := (len(realWebmBytes) + 2) / 3
	chunksData := [][]byte{
		realWebmBytes[:chunkSize],
		realWebmBytes[chunkSize : chunkSize*2],
		realWebmBytes[chunkSize*2:],
	}

	client := &http.Client{}
	for seq, chunkBytes := range chunksData {
		h := sha256.Sum256(chunkBytes)
		hHex := hex.EncodeToString(h[:])

		putURL := fmt.Sprintf("%s/api/v1/takes/%s/tracks/%s/chunks/%d", ts.URL, takeID, trackID, seq)
		req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(chunkBytes))
		req.Header.Set("Authorization", "Bearer "+uploadToken)
		req.Header.Set("X-Chunk-SHA256", hHex)

		putResp, err := client.Do(req)
		if err != nil || (putResp.StatusCode != http.StatusCreated && putResp.StatusCode != http.StatusOK) {
			t.Fatalf("Chunk %d PUT failed: %v", seq, err)
		}
	}

	// 7. Complete Track Upload with Sidecar
	compBody, _ := json.Marshal(upload.CompleteRequest{
		LastSeq:    2,
		TotalBytes: int64(len(chunksData[0]) + len(chunksData[1]) + len(chunksData[2])),
		Sidecar: map[string]interface{}{
			"trackId":    trackID,
			"kind":       "video",
			"sampleRate": 48000,
			"clockFit": map[string]interface{}{
				"a":        1000000.0,
				"b":        1.000012,
				"rttP10Ms": 14.5,
			},
			"events": []map[string]interface{}{
				{"type": "first-frame", "perfTime": 15.2, "mediaTimeUs": 0},
			},
		},
	})
	compURL := fmt.Sprintf("%s/api/v1/takes/%s/tracks/%s/complete", ts.URL, takeID, trackID)
	compReq, _ := http.NewRequest(http.MethodPost, compURL, bytes.NewReader(compBody))
	compReq.Header.Set("Authorization", "Bearer "+uploadToken)
	compResp, err := client.Do(compReq)
	if err != nil || compResp.StatusCode != http.StatusOK {
		t.Fatalf("Complete request failed: %v", err)
	}

	// Verify assembled raw file
	rawFile := filepath.Join(dataDir, "sessions", sessData.ID, "takes", takeID, "raw", trackID+".webm")
	if _, err := os.Stat(rawFile); os.IsNotExist(err) {
		t.Fatalf("Assembled raw webm file missing: %s", rawFile)
	}

	// 8. Trigger Post-processing
	postprocURL := fmt.Sprintf("%s/api/v1/takes/%s/postprocess", ts.URL, takeID)
	unauthorized, err := http.Post(postprocURL, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated postprocess=%d", unauthorized.StatusCode)
	}
	// Upload completion is independent of the recording stop command.
	tk.Status = "stopped"
	tk.StopAtHostUs = 2_000_000
	if err := st.UpdateTake(tk); err != nil {
		t.Fatal(err)
	}
	ppReq, err := http.NewRequest(http.MethodPost, postprocURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	ppReq.Header.Set("Authorization", "Bearer "+sessData.HostToken)
	ppResp, err := client.Do(ppReq)
	if err != nil {
		t.Fatal(err)
	}
	defer ppResp.Body.Close()
	if ppResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(ppResp.Body)
		t.Fatalf("Postprocess failed: status=%d body=%s", ppResp.StatusCode, body)
	}

	// 9. Verify Deliverables: manifest.json, FCPXML, and Premiere XML
	outDir := filepath.Join(dataDir, "sessions", sessData.ID, "takes", takeID, "out")
	manifestPath := filepath.Join(outDir, "manifest.json")
	fcpxmlPath := filepath.Join(outDir, "take_01_multicam.fcpxml")
	premierePath := filepath.Join(outDir, "take_01_premiere.xml")

	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest.json missing: %v", err)
	}
	if _, err := os.Stat(fcpxmlPath); err != nil {
		t.Fatalf("FCPXML missing: %v", err)
	}
	if _, err := os.Stat(premierePath); err != nil {
		t.Fatalf("Premiere XML missing: %v", err)
	}

	fcpxmlBytes, _ := os.ReadFile(fcpxmlPath)
	if !bytes.Contains(fcpxmlBytes, []byte("<multicam")) || !bytes.Contains(fcpxmlBytes, []byte("Take 1 Multicam")) {
		t.Fatalf("FCPXML missing multicam definition: %s", string(fcpxmlBytes))
	}

	premiereBytes, _ := os.ReadFile(premierePath)
	if !bytes.Contains(premiereBytes, []byte("<xmeml")) || !bytes.Contains(premiereBytes, []byte("Take 1 - Test Episode Multicam")) {
		t.Fatalf("Premiere XML missing expected sequence: %s", string(premiereBytes))
	}
}
