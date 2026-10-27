package postproc

import (
	"encoding/json"
	"math"
	"os"
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

func TestPortraitVideoKeepsGeometryAndDelayedFirstFrame(t *testing.T) {
	runner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess := &store.Session{ID: "portrait", Title: "Portrait fixture", CreatedAt: time.Now()}
	if err = st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	take := &store.Take{ID: "portrait-take", SessionID: sess.ID, Number: 1, StartAtHostUs: 1000000, Status: "stopped", WallClockStart: time.Now()}
	if err = st.CreateTake(take); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateParticipant(&store.Participant{ID: "phone", SessionID: sess.ID, Name: "Synthetic phone", Role: "guest", Status: "admitted", JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateSource(&store.Source{ID: "camera", ParticipantID: "phone", Kind: "camera", Label: "Portrait camera"}); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateTrack(&store.Track{ID: "video", TakeID: take.ID, SourceID: "camera", Kind: "video", Mime: "video/mp4", Status: "assembled"}); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(root, "sessions", sess.ID, "takes", take.ID, "raw")
	if err = os.MkdirAll(raw, 0700); err != nil {
		t.Fatal(err)
	}
	if err = runner.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc2=size=240x320:rate=30:duration=1", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=1.2", "-vf", "setpts=PTS+0.2/TB", "-fps_mode", "passthrough", "-c:v", "libx264", "-c:a", "aac", filepath.Join(raw, "video.mp4")}); err != nil {
		t.Fatal(err)
	}
	beta := 1.000005
	side := align.Sidecar{TrackID: "video", Kind: "video", ClockFit: align.ClockFitData{A: 1000000, B: beta, R2: 1, RttP10Ms: 1}, Events: []align.SyncEvent{{Type: "recorder-start", PerfTime: 0, MediaTimeUs: 0}}}
	data, _ := json.Marshal(side)
	if err = os.WriteFile(filepath.Join(raw, "video.sidecar.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	p := NewPipeline(st, &config.Config{DataDir: root}, runner)
	if err = p.ProcessTake(take.ID); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "sessions", sess.ID, "takes", take.ID, "out")
	data, err = os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest export.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var video, audio *export.ExportSource
	for i := range manifest.Sources {
		s := &manifest.Sources[i]
		if s.Kind == "video" {
			video = s
		} else if s.Kind == "audio" {
			audio = s
		}
	}
	if video == nil || audio == nil {
		t.Fatal("missing muxed source outputs")
	}
	if video.Width != 240 || video.Height != 320 {
		t.Fatalf("portrait geometry lost: %dx%d", video.Width, video.Height)
	}
	if math.Abs(video.OffsetSec-audio.OffsetSec-0.2*beta) > 0.002 {
		t.Fatalf("delayed first video frame lost: video=%f audio=%f", video.OffsetSec, audio.OffsetSec)
	}
	data, err = os.ReadFile(filepath.Join(out, "take_01_multicam.fcpxml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `width="240" height="320"`) {
		t.Fatal("portrait format missing from FCPXML")
	}
}
