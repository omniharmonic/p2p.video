package postproc

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Chromium's MP4/Opus recordings declare 80 ms of encoder priming without an
// edit list, so their first audible sample is presented at 0.08 s. The
// exported WAV starts at that sample; placed at the container's offset it ran
// 80 ms ahead of the camera's own video (rehearsal, [SL-QA-1]). The fixture
// is a synthetic click track recorded by Chromium's MediaRecorder, muxed
// (stream copy) with a tiny video.
func TestExportPlacesCameraAudioAfterOpusPriming(t *testing.T) {
	runner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "media", "chromium-mp4-opus-priming.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	session := &store.Session{ID: "prime", Title: "Priming", CreatedAt: now, PresetJSON: "{}"}
	take := &store.Take{ID: "take", SessionID: session.ID, Number: 1, StartAtHostUs: 1_000_000, Status: "stopped", WallClockStart: now}
	for _, err := range []error{
		st.CreateSession(session), st.CreateTake(take),
		st.CreateParticipant(&store.Participant{ID: "h", SessionID: session.ID, Name: "Host", Role: "host", Status: "admitted", JoinedAt: now}),
		st.CreateSource(&store.Source{ID: "cam", ParticipantID: "h", Kind: "camera", Label: "Camera"}),
		st.CreateTrack(&store.Track{ID: "video", TakeID: take.ID, SourceID: "cam", Kind: "video", Mime: "video/mp4", Status: "assembled"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "raw")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(raw, "video.mp4"), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	side, _ := json.Marshal(align.Sidecar{TrackID: "video", ClockFit: align.ClockFitData{A: 1_500_000, B: 1, R2: 1, RttP10Ms: 1}, Events: []align.SyncEvent{{Type: "recorder-start", PerfTime: 0}}})
	if err := os.WriteFile(filepath.Join(raw, "video.sidecar.json"), side, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewPipeline(st, &config.Config{DataDir: root}, runner).ProcessTake(take.ID); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "sessions", session.ID, "takes", take.ID, "out", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m export.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	var video, audio *export.ExportSource
	for i := range m.Sources {
		switch m.Sources[i].Kind {
		case "video":
			video = &m.Sources[i]
		case "audio":
			audio = &m.Sources[i]
		}
	}
	if video == nil || audio == nil {
		t.Fatalf("sources %+v", m.Sources)
	}
	if math.Abs(video.OffsetSec-0.5) > 0.002 {
		t.Fatalf("video offset %v", video.OffsetSec)
	}
	if d := audio.OffsetSec - video.OffsetSec; math.Abs(d-0.079) > 0.003 {
		t.Fatalf("camera audio placed %+.3f s after its video, want the 0.079 s Opus priming", d)
	}
}
