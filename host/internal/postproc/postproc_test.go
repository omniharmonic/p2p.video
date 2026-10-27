package postproc

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func TestSliceS3_RationalFrameRateConformity(t *testing.T) {
	tests := []struct {
		inputFPS     float64
		wantNTSC     bool
		wantNum      int64
		wantDen      int64
		wantTimebase int
		wantDurStr   string
	}{
		{29.97, true, 30000, 1001, 30, "1001/30000s"},
		{23.976, true, 24000, 1001, 24, "1001/24000s"},
		{59.94, true, 60000, 1001, 60, "1001/60000s"},
		{24.0, false, 24, 1, 24, "1/24s"},
		{25.0, false, 25, 1, 25, "1/25s"},
		{30.0, false, 30, 1, 30, "1/30s"},
		{60.0, false, 60, 1, 60, "1/60s"},
	}

	for _, tc := range tests {
		t.Run(fmt.Sprintf("FPS_%.3f", tc.inputFPS), func(t *testing.T) {
			r := export.ResolveFrameRate(tc.inputFPS)
			if r.IsNTSC != tc.wantNTSC {
				t.Errorf("IsNTSC = %v, want %v", r.IsNTSC, tc.wantNTSC)
			}
			if r.Numerator != tc.wantNum || r.Denominator != tc.wantDen {
				t.Errorf("Fraction = %d/%d, want %d/%d", r.Numerator, r.Denominator, tc.wantNum, tc.wantDen)
			}
			if r.Timebase != tc.wantTimebase {
				t.Errorf("Timebase = %d, want %d", r.Timebase, tc.wantTimebase)
			}
			if r.FrameDurStr != tc.wantDurStr {
				t.Errorf("FrameDurStr = %s, want %s", r.FrameDurStr, tc.wantDurStr)
			}
		})
	}
}

func TestSliceS3_SourcePairedPostProcessingAndNLEExport(t *testing.T) {
	// Skip if ffmpeg or ffprobe is not installed
	ffRunner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg not installed on test host, skipping full transcode slice")
	}

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Failed to initialize store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		DataDir: tempDir,
	}
	pipeline := NewPipeline(st, cfg, ffRunner)

	// Create Session and Take
	sess := &store.Session{
		ID:        "sess_s3",
		Title:     "S3 Editorial Test",
		CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateSession(sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	takeStartUs := time.Now().UnixNano() / 1000
	tk := &store.Take{
		ID:             "take_s3_01",
		SessionID:      sess.ID,
		Number:         1,
		StartAtHostUs:  takeStartUs,
		WallClockStart: time.Now().UTC(),
		Status:         "ready",
	}
	if err := st.CreateTake(tk); err != nil {
		t.Fatalf("CreateTake: %v", err)
	}

	part := &store.Participant{
		ID:        "p_alice_host",
		SessionID: sess.ID,
		Name:      "Alice Host",
		Role:      "host",
		Status:    "admitted",
		JoinedAt:  time.Now().UTC(),
	}
	if err := st.CreateParticipant(part); err != nil {
		t.Fatalf("CreateParticipant: %v", err)
	}

	guestPart := &store.Participant{
		ID:        "p_bob_guest",
		SessionID: sess.ID,
		Name:      "Bob Guest",
		Role:      "guest",
		Status:    "admitted",
		JoinedAt:  time.Now().UTC(),
	}
	if err := st.CreateParticipant(guestPart); err != nil {
		t.Fatalf("CreateGuestParticipant: %v", err)
	}

	guestCamSrc := &store.Source{
		ID:            "src_bob_cam",
		ParticipantID: guestPart.ID,
		Kind:          "camera",
		Label:         "Bob FaceCam",
	}
	if err := st.CreateSource(guestCamSrc); err != nil {
		t.Fatalf("CreateGuestCamSource: %v", err)
	}

	guestMicSrc := &store.Source{
		ID:            "src_bob_mic",
		ParticipantID: guestPart.ID,
		Kind:          "mic",
		Label:         "Bob Shure SM7B",
	}
	if err := st.CreateSource(guestMicSrc); err != nil {
		t.Fatalf("CreateGuestMicSource: %v", err)
	}

	// Create directories
	takeDir := filepath.Join(tempDir, "sessions", sess.ID, "takes", tk.ID)
	rawDir := filepath.Join(takeDir, "raw")
	refsDir := filepath.Join(takeDir, "refs")
	if err := os.MkdirAll(rawDir, 0755); err != nil {
		t.Fatalf("MkdirAll rawDir: %v", err)
	}
	if err := os.MkdirAll(refsDir, 0755); err != nil {
		t.Fatalf("MkdirAll refsDir: %v", err)
	}

	// 1. Generate real muxed video file with both video and audio stream: track1 (guest camera + embedded audio)
	tr1 := &store.Track{
		ID:       "trk_bob_cam",
		TakeID:   tk.ID,
		SourceID: guestCamSrc.ID,
		Kind:     "video",
		Mime:     "video/webm",
		Status:   "assembled",
	}
	if err := st.CreateTrack(tr1); err != nil {
		t.Fatalf("CreateTrack1: %v", err)
	}
	rawVideoPath := filepath.Join(rawDir, tr1.ID+".webm")
	genVideoCmd := exec.Command(ffRunner.FFmpegPath,
		"-y",
		"-f", "lavfi", "-i", "testsrc=duration=3:size=640x360:rate=30",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libvpx", "-b:v", "500k",
		"-c:a", "libopus", "-b:a", "64k",
		rawVideoPath,
	)
	if out, err := genVideoCmd.CombinedOutput(); err != nil {
		t.Fatalf("Generate test webm failed: %v, out: %s", err, string(out))
	}

	// 2. Generate isolated audio file: track2 (guest mic)
	tr2 := &store.Track{
		ID:       "trk_bob_mic",
		TakeID:   tk.ID,
		SourceID: guestMicSrc.ID,
		Kind:     "audio",
		Mime:     "audio/wav",
		Status:   "assembled",
	}
	if err := st.CreateTrack(tr2); err != nil {
		t.Fatalf("CreateTrack2: %v", err)
	}
	rawAudioPath := filepath.Join(rawDir, tr2.ID+".wav")
	genAudioCmd := exec.Command(ffRunner.FFmpegPath,
		"-y",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=3",
		"-c:a", "pcm_s16le", "-ar", "48000",
		rawAudioPath,
	)
	if out, err := genAudioCmd.CombinedOutput(); err != nil {
		t.Fatalf("Generate test wav failed: %v, out: %s", err, string(out))
	}

	// 3. Generate source-paired reference recording for track2 in refsDir: refs/{sourceID}.ogg
	refAudioPath := filepath.Join(refsDir, guestMicSrc.ID+".ogg")
	genRefCmd := exec.Command(ffRunner.FFmpegPath,
		"-y",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=3",
		"-c:a", "libopus", "-b:a", "64k",
		refAudioPath,
	)
	if out, err := genRefCmd.CombinedOutput(); err != nil {
		t.Fatalf("Generate test ref ogg failed: %v, out: %s", err, string(out))
	}

	// 4. Create truthful clock sidecars for both tracks
	sidecar1 := align.Sidecar{
		TrackID: tr1.ID,
		Kind:    "video",
		ClockFit: align.ClockFitData{
			A:        float64(takeStartUs) + 50000.0, // 50ms offset
			B:        1.000005,                       // 5 ppm drift
			R2:       0.998,
			RttP10Ms: 12.0,
		},
		Events: []align.SyncEvent{
			{Type: "first-frame", PerfTime: 100.0, MediaTimeUs: 0},
		},
	}
	sidecar1Bytes, _ := json.Marshal(sidecar1)
	// The camera's muxed mic was recorded "clean" [SL-P1-10]: the editor learns it from the manifest.
	var side1 map[string]any
	_ = json.Unmarshal(sidecar1Bytes, &side1)
	side1["micProcessing"] = map[string]any{"mode": "auto", "resolved": "clean", "reason": "no_headphones", "headphones": false, "echoCancellation": true, "noiseSuppression": true, "autoGainControl": false}
	sidecar1Bytes, _ = json.Marshal(side1)
	_ = os.WriteFile(filepath.Join(rawDir, tr1.ID+".sidecar.json"), sidecar1Bytes, 0644)

	sidecar2 := align.Sidecar{
		TrackID: tr2.ID,
		Kind:    "audio",
		ClockFit: align.ClockFitData{
			A:        float64(takeStartUs) + 20000.0, // 20ms offset
			B:        1.000002,                       // 2 ppm drift
			R2:       0.999,
			RttP10Ms: 10.0,
		},
		Events: []align.SyncEvent{
			{Type: "first-sample", PerfTime: 100.0, MediaTimeUs: 0},
		},
	}
	sidecar2Bytes, _ := json.Marshal(sidecar2)
	_ = os.WriteFile(filepath.Join(rawDir, tr2.ID+".sidecar.json"), sidecar2Bytes, 0644)

	// Snapshot a second recorder whose capture failed before track registration.
	// The valid guest tracks must not hide that missing participant.
	if err := st.ExpectTakeParticipant(tk.ID, part.ID); err != nil {
		t.Fatal(err)
	}
	if err := pipeline.ProcessTake(tk.ID); err == nil || !strings.Contains(err.Error(), "expected participants") {
		t.Fatalf("missing recorder hidden: %v", err)
	}
	// Recovery supplies the missing recorder's assembled local audio and timing.
	recoveredSource := &store.Source{ID: "host_mic", ParticipantID: part.ID, Kind: "mic", Label: "Host Mic"}
	if err := st.CreateSource(recoveredSource); err != nil {
		t.Fatal(err)
	}
	recoveredTrack := &store.Track{ID: "host_audio", TakeID: tk.ID, SourceID: recoveredSource.ID, Kind: "audio", Mime: "audio/wav", Status: "assembled"}
	if err := st.CreateTrack(recoveredTrack); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(rawAudioPath, filepath.Join(rawDir, recoveredTrack.ID+".wav")); err != nil {
		t.Fatal(err)
	}
	recoveredSidecar := sidecar2
	recoveredSidecar.TrackID = recoveredTrack.ID
	recoveredBytes, err := json.Marshal(recoveredSidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rawDir, recoveredTrack.ID+".sidecar.json"), recoveredBytes, 0600); err != nil {
		t.Fatal(err)
	}

	// The host's lower third reaches the manifest [SL-P5-2].
	if err := st.SetLowerThird(tk.SessionID, store.LowerThird{ParticipantID: part.ID, Name: "Ana Ruiz", Title: "Founder, X"}); err != nil {
		t.Fatal(err)
	}

	// Execute post-processing pipeline
	if err := pipeline.ProcessTake(tk.ID); err != nil {
		t.Fatalf("ProcessTake failed: %v", err)
	}

	// Verify Take status in DB
	updatedTake, err := st.GetTake(tk.ID)
	if err != nil {
		t.Fatalf("GetTake: %v", err)
	}
	if updatedTake.Status != "complete" {
		t.Errorf("Take status = %s, want complete", updatedTake.Status)
	}

	outDir := filepath.Join(takeDir, "out")
	manifestPath := filepath.Join(outDir, "manifest.json")
	fcpxmlPath := filepath.Join(outDir, "take_01_multicam.fcpxml")
	xmemlPath := filepath.Join(outDir, "take_01_premiere.xml")

	// Verify manifest.json exists and is valid
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("Read manifest.json: %v", err)
	}
	var manifest export.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("Parse manifest.json: %v", err)
	}
	if manifest.Schema != "studiolink.take/1" {
		t.Errorf("Manifest schema = %s, want studiolink.take/1", manifest.Schema)
	}
	if len(manifest.Sources) == 0 {
		t.Fatalf("Manifest has zero sources")
	}

	// Verify audio extraction from video:
	// Since tr1 was a muxed video with audio, it should have produced BOTH a video export and an extracted audio export
	hasExtractedAudio := false
	for _, src := range manifest.Sources {
		if strings.Contains(src.Label, "Audio") && src.Kind == "audio" {
			hasExtractedAudio = true
		}
	}
	if !hasExtractedAudio {
		t.Errorf("Expected audio to be extracted from muxed video track, but not found in manifest sources: %+v", manifest.Sources)
	}
	// Recording mic processing reaches the manifest on the mic's audio only [SL-P1-10].
	for _, src := range manifest.Sources {
		switch {
		case src.ID == tr1.ID+"-audio":
			if m := src.MicProcessing; m == nil || m.Resolved != "clean" || !m.EchoCancellation || !m.NoiseSuppression || m.AutoGainControl || m.Reason != "no_headphones" {
				t.Errorf("camera mic processing in manifest: %+v", m)
			}
		case src.MicProcessing != nil:
			t.Errorf("%s (%s) carries mic processing it never declared: %+v", src.ID, src.Kind, src.MicProcessing)
		}
	}
	if !strings.Contains(string(manifestBytes), `"micProcessing"`) {
		t.Error("manifest.json lacks micProcessing")
	}
	foundTitle := false
	for _, mp := range manifest.Participants {
		if mp.ID == part.ID {
			foundTitle = mp.LowerThirdName == "Ana Ruiz" && mp.Title == "Founder, X" && mp.Name == part.Name
		} else if mp.LowerThirdName != mp.Name || mp.Title != "" {
			t.Errorf("participant without a lower third: %+v", mp)
		}
	}
	if !foundTitle || len(manifest.Participants) < 2 {
		t.Errorf("manifest participants lack the lower third: %+v", manifest.Participants)
	}

	// Verify FCPXML syntax and multicam elements
	fcpxmlBytes, err := os.ReadFile(fcpxmlPath)
	if err != nil {
		t.Fatalf("Read FCPXML: %v", err)
	}
	var fcpxmlDoc struct {
		XMLName xml.Name `xml:"fcpxml"`
		Version string   `xml:"version,attr"`
	}
	if err := xml.Unmarshal(fcpxmlBytes, &fcpxmlDoc); err != nil {
		t.Fatalf("FCPXML is not valid XML: %v", err)
	}
	if fcpxmlDoc.Version != "1.10" {
		t.Errorf("FCPXML version = %s, want 1.10", fcpxmlDoc.Version)
	}
	fcpxmlStr := string(fcpxmlBytes)
	if !strings.Contains(fcpxmlStr, "<multicam") {
		t.Errorf("FCPXML missing <multicam> element")
	}
	if !strings.Contains(fcpxmlStr, "<mc-angle") {
		t.Errorf("FCPXML missing <mc-angle> element")
	}

	// Verify XMEML Premiere / Resolve syntax
	xmemlBytes, err := os.ReadFile(xmemlPath)
	if err != nil {
		t.Fatalf("Read XMEML: %v", err)
	}
	var xmemlDoc struct {
		XMLName xml.Name `xml:"xmeml"`
		Version string   `xml:"version,attr"`
	}
	if err := xml.Unmarshal(xmemlBytes, &xmemlDoc); err != nil {
		t.Fatalf("XMEML is not valid XML: %v", err)
	}
	if xmemlDoc.Version != "4" {
		t.Errorf("XMEML version = %s, want 4", xmemlDoc.Version)
	}
	xmemlStr := string(xmemlBytes)
	if !strings.Contains(xmemlStr, "<video>") || !strings.Contains(xmemlStr, "<audio>") {
		t.Errorf("XMEML missing separate <video> and <audio> sections")
	}

	// Verify output media files with ffprobe
	// 1. Verify conformed CFR video
	t.Logf("manifest.Sources = %+v", manifest.Sources)
	var conformedVideoPath string
	for _, src := range manifest.Sources {
		if src.Kind == "video" {
			conformedVideoPath = filepath.Join(outDir, src.FileRelPath)
			break
		}
	}
	if conformedVideoPath == "" {
		t.Fatalf("No conformed video found")
	}
	dur, err := ffRunner.ProbeDurationSec(conformedVideoPath)
	if err != nil || dur <= 0 {
		t.Errorf("Probe conformed video duration failed: %v, dur: %f", err, dur)
	}
	hasAudioInCFRVideo, _ := ffRunner.HasAudioStream(conformedVideoPath)
	if hasAudioInCFRVideo {
		t.Errorf("Conformed CFR video contains audio, expected -an video conform")
	}

	// 2. Verify conformed 24-bit 48kHz WAV audio
	var conformedAudioPath string
	for _, src := range manifest.Sources {
		if src.Kind == "audio" {
			conformedAudioPath = filepath.Join(outDir, src.FileRelPath)
			break
		}
	}
	if conformedAudioPath == "" {
		t.Fatalf("No conformed audio found")
	}
	aDur, err := ffRunner.ProbeDurationSec(conformedAudioPath)
	if err != nil || aDur <= 0 {
		t.Errorf("Probe conformed audio duration failed: %v, dur: %f", err, aDur)
	}

	// Verify every deliverable was published under the isolated data directory,
	// with measured clock timing and real decoded audio/video metadata.
	exportDir := filepath.Join(tempDir, "exports", sanitize(sess.Title)+"_"+sess.ID, fmt.Sprintf("take_%02d_%s", tk.Number, tk.ID))
	for _, source := range manifest.Sources {
		if source.Method != "sidecar-coarse" {
			t.Fatalf("unsupported alignment claim: %s", source.Method)
		}
		published := filepath.Join(exportDir, source.FileRelPath)
		info, err := os.Stat(published)
		if err != nil || info.Size() == 0 {
			t.Fatalf("missing published media %s: %v", published, err)
		}
		entries := "stream=codec_name,sample_rate,avg_frame_rate"
		metadata, err := exec.Command(ffRunner.FFprobePath, "-v", "error", "-show_entries", entries, "-of", "json", published).Output()
		if err != nil {
			t.Fatal(err)
		}
		if source.Kind == "audio" && (!strings.Contains(string(metadata), "pcm_s24le") || !strings.Contains(string(metadata), "48000")) {
			t.Fatalf("wrong WAV format: %s", metadata)
		}
		if source.Kind == "video" && (!strings.Contains(string(metadata), "h264") || !strings.Contains(string(metadata), manifest.FrameRate)) {
			t.Fatalf("wrong CFR video: %s", metadata)
		}
	}

	// A destination failure must fail the take after real transcoding; it cannot
	// leave a success state just because local working media exists.
	blocker := filepath.Join(tempDir, "blocked-export")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.ExportDir = blocker
	if err := pipeline.ProcessTake(tk.ID); err == nil {
		t.Fatal("export copy failure reported complete")
	}
	failed, err := st.GetTake(tk.ID)
	if err != nil || failed.Status != "failed" {
		t.Fatalf("failed export status=%+v err=%v", failed, err)
	}

	_ = part
}

func TestSliceS3_MissingFFmpegFailsGracefully(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{DataDir: tempDir}
	// Pipeline with nil FFmpeg runner
	pipeline := NewPipeline(st, cfg, nil)

	sess := &store.Session{ID: "sess_fail", Title: "Fail Test", CreatedAt: time.Now().UTC()}
	_ = st.CreateSession(sess)
	tk := &store.Take{ID: "take_fail", SessionID: sess.ID, Number: 1, StartAtHostUs: time.Now().UnixNano() / 1000, WallClockStart: time.Now().UTC(), Status: "ready"}
	_ = st.CreateTake(tk)
	part := &store.Participant{ID: "p_alice", SessionID: sess.ID, Name: "Alice", Role: "host", Status: "admitted", JoinedAt: time.Now().UTC()}
	_ = st.CreateParticipant(part)
	src := &store.Source{ID: "src_mic", ParticipantID: part.ID, Kind: "mic", Label: "Mic"}
	_ = st.CreateSource(src)
	tr := &store.Track{ID: "tr_mic", TakeID: tk.ID, SourceID: src.ID, Kind: "audio", Mime: "audio/wav", Status: "assembled"}
	_ = st.CreateTrack(tr)

	takeDir := filepath.Join(tempDir, "sessions", sess.ID, "takes", tk.ID)
	rawDir := filepath.Join(takeDir, "raw")
	_ = os.MkdirAll(rawDir, 0755)
	_ = os.WriteFile(filepath.Join(rawDir, tr.ID+".wav"), []byte("dummy raw audio"), 0644)

	err = pipeline.ProcessTake(tk.ID)
	if err == nil {
		t.Fatal("Expected error with nil FFmpeg, got nil")
	}

	updated, _ := st.GetTake(tk.ID)
	if updated.Status != "failed" {
		t.Errorf("Take status = %s, want failed", updated.Status)
	}
}
