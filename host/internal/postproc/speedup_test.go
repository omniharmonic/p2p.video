package postproc

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

func TestRunJobsBoundsConcurrencyAndCancelsOnFirstFailure(t *testing.T) {
	var running, peak, cancelled atomic.Int32
	track := func() func() {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		return func() { running.Add(-1) }
	}
	var jobs []mediaJob
	for i := 0; i < 6; i++ {
		jobs = append(jobs, mediaJob{video: true, run: func(ctx context.Context) error {
			defer track()()
			time.Sleep(20 * time.Millisecond)
			return nil
		}})
	}
	if err := runJobs(jobs, 2, 1); err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 {
		t.Fatalf("peak video concurrency %d, want 2", peak.Load())
	}
	boom := errors.New("encode failed")
	jobs = []mediaJob{
		{video: true, run: func(ctx context.Context) error { time.Sleep(10 * time.Millisecond); return boom }},
		{video: true, run: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				cancelled.Add(1)
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		}},
	}
	start := time.Now()
	if err := runJobs(jobs, 2, 1); !errors.Is(err, boom) {
		t.Fatalf("first failure lost: %v", err)
	}
	if cancelled.Load() != 1 || time.Since(start) > 2*time.Second {
		t.Fatal("sibling job was not cancelled")
	}
}

func TestProgressWeightsMediaAndFinalization(t *testing.T) {
	r := newRun("take")
	r.addJob("video", 100)
	r.addJob("audio", 5)
	_ = r.timed(StageVideo, func() error {
		if s := r.snapshot(); s.Stage != StageVideo {
			t.Errorf("stage %q while encoding", s.Stage)
		}
		r.jobProgress("video", 0.5)
		return nil
	})
	r.jobProgress("video", 0.25) // never moves backwards
	if s := r.snapshot(); math.Abs(s.Percent-45.2) > 0.1 || s.State != "running" {
		t.Fatalf("snapshot %+v", s)
	}
	r.finish(errors.New("transcode video x: ffmpeg failed: exit status 1, stderr: secret path\nmore"))
	s := r.snapshot()
	if s.State != "failed" || s.Error != "transcode video x: ffmpeg failed: exit status 1" || len(s.Stages) != 1 || s.Stages[0].Jobs != 1 {
		t.Fatalf("failed snapshot %+v", s)
	}
	ok := newRun("take")
	ok.addJob("a", 1)
	ok.finish(nil)
	if s := ok.snapshot(); s.Percent != 100 || s.State != "complete" {
		t.Fatalf("complete snapshot %+v", s)
	}
}

// One camera is exact-CFR H.264 (copied), one is MediaRecorder-like VFR VP8
// (conformed). Re-exporting as ProRes Proxy re-encodes both, keeps timing and
// preserves the previous package.
func TestParallelExportStreamCopyAndProRes(t *testing.T) {
	runner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	session := &store.Session{ID: "speed", Title: "Speed", CreatedAt: now, PresetJSON: "{}"}
	take := &store.Take{ID: "take", SessionID: session.ID, Number: 1, StartAtHostUs: 1_000_000, Status: "stopped", WallClockStart: now}
	for _, err := range []error{
		st.CreateSession(session), st.CreateTake(take),
		st.CreateParticipant(&store.Participant{ID: "a", SessionID: session.ID, Name: "Ana", Role: "host", Status: "admitted", JoinedAt: now}),
		st.CreateParticipant(&store.Participant{ID: "b", SessionID: session.ID, Name: "Ben", Role: "guest", Status: "admitted", JoinedAt: now}),
		st.CreateSource(&store.Source{ID: "a-cam", ParticipantID: "a", Kind: "camera", Label: "Camera"}),
		st.CreateSource(&store.Source{ID: "b-cam", ParticipantID: "b", Kind: "camera", Label: "Camera"}),
		st.CreateTrack(&store.Track{ID: "cfr", TakeID: take.ID, SourceID: "a-cam", Kind: "video", Mime: "video/mp4", Status: "assembled"}),
		st.CreateTrack(&store.Track{ID: "vfr", TakeID: take.ID, SourceID: "b-cam", Kind: "video", Mime: "video/webm", Status: "assembled"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "raw")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runner.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=30:duration=3", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=3", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", filepath.Join(raw, "cfr.mp4")}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=20:duration=3", "-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000:duration=3", "-vf", "setpts='(N/20+(random(1)-0.5)*0.008)/TB'", "-fps_mode", "passthrough", "-c:v", "libvpx", "-b:v", "300k", "-c:a", "libopus", filepath.Join(raw, "vfr.webm")}); err != nil {
		t.Fatal(err)
	}
	for id, beta := range map[string]float64{"cfr": 1, "vfr": 1.0000125} {
		data, _ := json.Marshal(align.Sidecar{TrackID: id, ClockFit: align.ClockFitData{A: 1_100_000, B: beta, R2: 1, RttP10Ms: 1}, Events: []align.SyncEvent{{Type: "recorder-start", PerfTime: 0}}})
		if err := os.WriteFile(filepath.Join(raw, id+".sidecar.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pipe := NewPipeline(st, &config.Config{DataDir: root}, runner)
	out := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "out")
	check := func(wantCodec string, wantCopies int) export.Manifest {
		t.Helper()
		if err := pipe.ProcessTake(take.ID); err != nil {
			t.Fatal(err)
		}
		status, ok := pipe.ExportStatus(take.ID)
		if !ok || status.State != "complete" || status.Percent != 100 || status.StreamCopies != wantCopies || status.VideoCodec != ffmpeg.NormalizeCodec(wantCodec) {
			t.Fatalf("export status %+v", status)
		}
		data, err := os.ReadFile(filepath.Join(out, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var m export.Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		// Track order survives parallel jobs: each camera's audio, then video.
		var ids []string
		for _, s := range m.Sources {
			ids = append(ids, s.ID)
			info, err := runner.ProbeMedia(filepath.Join(out, s.FileRelPath))
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(info.DurationSec-3) > 0.1 || math.Abs(s.DurationSec-info.DurationSec) > 1e-6 {
				t.Errorf("%s lasts %.3fs (manifest %.3fs)", s.ID, info.DurationSec, s.DurationSec)
			}
			if s.Kind == "video" {
				codec := "h264"
				if wantCodec == ffmpeg.CodecProResProxy {
					codec = "prores"
				}
				if info.VideoCodec != codec || info.Video.Width != 320 || s.Width != 320 || info.FrameRate != "30/1" {
					t.Errorf("%s video %+v", s.ID, info)
				}
				if ok, why := runner.CFRCopyEligible(info, filepath.Join(out, s.FileRelPath), 30, 1, ffmpeg.CodecH264); codec == "h264" && !ok {
					t.Errorf("%s is not CFR: %s", s.ID, why)
				}
			}
		}
		if strings.Join(ids, ",") != "cfr-audio,cfr,vfr-audio,vfr" {
			t.Fatalf("manifest order %v", ids)
		}
		return m
	}
	m := check(ffmpeg.CodecH264, 1)
	if m.Sources[1].OffsetSec != m.Sources[0].OffsetSec {
		t.Fatal("copied camera lost its A/V start relationship")
	}
	if err := st.UpdateSessionSettings(session.ID, `{"export_codec":"prores_proxy","target_fps":30}`); err != nil {
		t.Fatal(err)
	}
	check(ffmpeg.CodecProResProxy, 0)
	previous, _ := filepath.Glob(filepath.Join(root, "exports", "*", "take_01_take.previous-*"))
	if len(previous) != 1 {
		t.Fatalf("previous package not preserved: %v", previous)
	}
}

func TestOriginalsPackageIsReadyBeforeExport(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)
	session := &store.Session{ID: "orig", Title: "Episode / 1", CreatedAt: now, PresetJSON: "{}"}
	take := &store.Take{ID: "take", SessionID: session.ID, Number: 2, StartAtHostUs: 1_000_000, Status: "stopped", WallClockStart: now}
	for _, err := range []error{
		st.CreateSession(session), st.CreateTake(take),
		st.CreateParticipant(&store.Participant{ID: "a", SessionID: session.ID, Name: "Ana", Role: "host", Status: "admitted", JoinedAt: now}),
		st.CreateSource(&store.Source{ID: "cam", ParticipantID: "a", Kind: "camera", Label: "Camera"}),
		st.CreateSource(&store.Source{ID: "mic", ParticipantID: "a", Kind: "mic", Label: "Mic"}),
		st.CreateTrack(&store.Track{ID: "video", TakeID: take.ID, SourceID: "cam", Kind: "video", Mime: "video/webm;codecs=vp9,opus", Status: "assembled", SHA256: "abc"}),
		st.CreateTrack(&store.Track{ID: "pcm", TakeID: take.ID, SourceID: "mic", Kind: "audio", Mime: "audio/wav", Status: "uploading"}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "raw")
	if err := os.MkdirAll(raw, 0o700); err != nil {
		t.Fatal(err)
	}
	media := map[string][]byte{"video.webm": bytes.Repeat([]byte("v"), 1000), "pcm.wav": bytes.Repeat([]byte("p"), 500)}
	for name, data := range media {
		if err := os.WriteFile(filepath.Join(raw, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	complete := true
	for id, extra := range map[string]map[string]any{"video": nil, "pcm": {"captureComplete": &complete, "replacesAudioOfTrackId": "video", "pcm": map[string]any{"format": "float32le", "sampleRate": 48000, "channels": 1, "frames": 100}}} {
		side := map[string]any{"trackId": id, "clockFit": align.ClockFitData{A: 1_250_000, B: 1, R2: 1, RttP10Ms: 1}, "events": []align.SyncEvent{{Type: "recorder-start", PerfTime: 0}}}
		for k, v := range extra {
			side[k] = v
		}
		data, _ := json.Marshal(side)
		if err := os.WriteFile(filepath.Join(raw, id+".sidecar.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pipe := NewPipeline(st, &config.Config{DataDir: root}, nil)
	if _, err := pipe.PlanOriginals(take.ID); err == nil || !strings.Contains(err.Error(), "awaiting assembly") {
		t.Fatalf("unassembled track allowed: %v", err)
	}
	pcmTrack, err := st.GetTrack("pcm")
	if err != nil {
		t.Fatal(err)
	}
	pcmTrack.Status = "assembled"
	if err := st.UpdateTrack(pcmTrack); err != nil {
		t.Fatal(err)
	}
	// The export lock is irrelevant: originals never wait for an export.
	pipe.mu.Lock()
	pkg, err := pipe.PlanOriginals(take.ID)
	pipe.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	files, err := pkg.OpenFiles()
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := pkg.WriteZip(&buf, files); err != nil {
		t.Fatal(err)
	}
	CloseFiles(files)
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = data
	}
	if !bytes.Equal(got["originals/Ana_Camera_video.webm"], media["video.webm"]) || !bytes.Equal(got["originals/Ana_Mic_pcm.wav"], media["pcm.wav"]) || len(got) != 5 {
		t.Fatalf("zip entries %v", keys(got))
	}
	var m OriginalsManifest
	if err := json.Unmarshal(got["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	if m.Schema != OriginalsSchema || m.Take != 2 || len(m.Files) != 2 || math.Abs(m.Files[0].OffsetSec-0.25) > 1e-9 || m.Files[0].SHA256 != "abc" || m.Files[1].ReplacesAudioOf != "video" || m.Files[0].Bytes != 1000 {
		t.Fatalf("manifest %+v", m)
	}
	var offsets map[string]float64
	if err := json.Unmarshal(got["offsets.json"], &offsets); err != nil || math.Abs(offsets["originals/Ana_Mic_pcm.wav"]-0.25) > 1e-9 {
		t.Fatalf("offsets %v %v", offsets, err)
	}
	// A file that changes after planning fails instead of shipping a mismatch.
	if err := os.WriteFile(filepath.Join(raw, "pcm.wav"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, _ = pkg.OpenFiles()
	defer CloseFiles(files)
	if err := pkg.WriteZip(io.Discard, files); err == nil {
		t.Fatal("changed original packaged")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
