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

// A screen share started 2 s into the take, with an odd-sized window that is
// resized mid-recording (as browser window capture does), is exported as its
// own video-only angle at its sidecar offset next to the camera angle. The
// expected recorder is satisfied by the camera, not the screen.
func TestLateScreenShareExportsAsOwnAngle(t *testing.T) {
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
	sess := &store.Session{ID: "screen", Title: "Screen fixture", CreatedAt: time.Now()}
	if err = st.CreateSession(sess); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateParticipant(&store.Participant{ID: "ana", SessionID: sess.ID, Name: "Ana", Role: "guest", Status: "admitted", JoinedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	take := &store.Take{ID: "screen-take", SessionID: sess.ID, Number: 1, StartAtHostUs: 1_000_000, Status: "stopped", WallClockStart: time.Now()}
	if err = st.CreateTakeWithParticipants(take, []string{"ana"}); err != nil {
		t.Fatal(err)
	}
	for _, src := range []store.Source{{ID: "cam", ParticipantID: "ana", Kind: "camera", Label: "Camera + microphone"}, {ID: "scr", ParticipantID: "ana", Kind: "screen", Label: "Screen"}} {
		if err = st.CreateSource(&src); err != nil {
			t.Fatal(err)
		}
	}
	for _, tr := range []store.Track{{ID: "camera", TakeID: take.ID, SourceID: "cam", Kind: "video", Mime: "video/mp4", Status: "assembled"}, {ID: "screen", TakeID: take.ID, SourceID: "scr", Kind: "video", Mime: "video/webm", Status: "assembled"}} {
		if err = st.CreateTrack(&tr); err != nil {
			t.Fatal(err)
		}
	}
	raw := filepath.Join(root, "sessions", sess.ID, "takes", take.ID, "raw")
	if err = os.MkdirAll(raw, 0700); err != nil {
		t.Fatal(err)
	}
	if err = runner.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=4", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=4", "-c:v", "libx264", "-c:a", "aac", filepath.Join(raw, "camera.mp4")}); err != nil {
		t.Fatal(err)
	}
	// Screen: odd-sized, then resized mid-stream; VP8 like Chrome's MediaRecorder.
	segA, segB, list := filepath.Join(root, "a.webm"), filepath.Join(root, "b.webm"), filepath.Join(root, "list.txt")
	if err = runner.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc=size=321x241:rate=15:duration=1", "-c:v", "libvpx", "-b:v", "500k", segA}); err != nil {
		t.Skip("libvpx unavailable in this ffmpeg build")
	}
	if err = runner.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc=size=400x300:rate=15:duration=1", "-c:v", "libvpx", "-b:v", "500k", segB}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(list, []byte("file '"+segA+"'\nfile '"+segB+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = runner.Execute([]string{"-y", "-f", "concat", "-safe", "0", "-i", list, "-c", "copy", filepath.Join(raw, "screen.webm")}); err != nil {
		t.Fatal(err)
	}
	for id, perfMs := range map[string]float64{"camera": 0, "screen": 2000} {
		side := align.Sidecar{TrackID: id, Kind: "video", ClockFit: align.ClockFitData{A: 1_000_000, B: 1, R2: 1, RttP10Ms: 1}, Events: []align.SyncEvent{{Type: "recorder-start", PerfTime: perfMs}}}
		data, _ := json.Marshal(side)
		if err = os.WriteFile(filepath.Join(raw, id+".sidecar.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	p := NewPipeline(st, &config.Config{DataDir: root}, runner)
	if err = p.ProcessTake(take.ID); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(root, "sessions", sess.ID, "takes", take.ID, "out")
	data, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest export.Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var screen, camera *export.ExportSource
	for i := range manifest.Sources {
		s := &manifest.Sources[i]
		if s.Kind != "video" {
			if s.Source == export.SourceScreen {
				t.Fatalf("silent screen produced an audio deliverable: %+v", s)
			}
			continue
		}
		if s.Source == export.SourceScreen {
			screen = s
		} else {
			camera = s
		}
	}
	if screen == nil || camera == nil {
		t.Fatalf("missing angles in manifest: %+v", manifest.Sources)
	}
	if math.Abs(screen.OffsetSec-2.0) > 0.01 || math.Abs(camera.OffsetSec) > 0.01 {
		t.Fatalf("offsets: screen=%f camera=%f", screen.OffsetSec, camera.OffsetSec)
	}
	if screen.Width != 320 || screen.Height != 240 {
		t.Fatalf("screen canvas %dx%d, want even 320x240 from the initial 321x241", screen.Width, screen.Height)
	}
	if screen.DurationSec < 1.8 {
		t.Fatalf("resized screen recording truncated: %fs", screen.DurationSec)
	}
	fcp, err := os.ReadFile(filepath.Join(out, "take_01_multicam.fcpxml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(fcp), `name="Ana Screen"`) || !strings.Contains(string(fcp), `name="Ana Camera"`) {
		t.Fatal("multicam lacks separate camera and screen angles")
	}
	// The decoded screen deliverable has frames.
	if _, err := runner.ExtractAudioSamples(filepath.Join(out, screen.FileRelPath), 48000, 0.1); err == nil {
		t.Fatal("video-only screen deliverable unexpectedly has audio")
	}
}
