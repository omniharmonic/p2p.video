package postproc

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// TestExportBenchmark is the reproducible export benchmark [SL-P0-9]. It is
// opt-in (STUDIOLINK_BENCH=1) because a realistic take is minutes of media.
//
// Each participant gets a MediaRecorder-like camera track: VP9 + Opus in
// fragmented MP4 (what Chromium records for video/mp4), ~0.8 Mbit/s, ~20 fps
// with jittered (VFR) timestamps, plus a sidecar with a 12.5 ppm clock drift.
// One synthesized minute is concatenated (stream copy) to the take length and
// hard-linked per participant, so synthesis stays cheap at 30+ minutes.
//
//	STUDIOLINK_BENCH_SOURCES   participants (default 3)
//	STUDIOLINK_BENCH_MINUTES   take length (default 2)
//	STUDIOLINK_BENCH_SIZE      camera geometry (default 1920x1080)
//	STUDIOLINK_BENCH_PCM=1     add each participant's isolated PCM microphone
//	STUDIOLINK_BENCH_SCREEN=1  add a screen share from the first participant
//	STUDIOLINK_BENCH_CODEC     session export_codec (default: session default)
//	STUDIOLINK_BENCH_VIDEO_WORKERS  override concurrent video encodes
//	STUDIOLINK_BENCH_CACHE     keep synthesized sources here between runs
//	STUDIOLINK_BENCH_OUT       write the JSON report here
//
// Example: STUDIOLINK_BENCH=1 STUDIOLINK_BENCH_MINUTES=10 go test ./internal/postproc -run TestExportBenchmark -v -timeout 2h
func TestExportBenchmark(t *testing.T) {
	if os.Getenv("STUDIOLINK_BENCH") != "1" {
		t.Skip("set STUDIOLINK_BENCH=1 to run the export benchmark")
	}
	runner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	sources := benchInt(t, "STUDIOLINK_BENCH_SOURCES", 3)
	minutes := benchFloat(t, "STUDIOLINK_BENCH_MINUTES", 2)
	size := benchEnv("STUDIOLINK_BENCH_SIZE", "1920x1080")
	pcm := os.Getenv("STUDIOLINK_BENCH_PCM") == "1"
	screen := os.Getenv("STUDIOLINK_BENCH_SCREEN") == "1"
	cache := os.Getenv("STUDIOLINK_BENCH_CACHE")
	if cache == "" {
		cache = t.TempDir()
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	synthStart := time.Now()
	camera := benchCamera(t, runner, cache, size, minutes)
	synthSec := time.Since(synthStart).Seconds()

	root := t.TempDir()
	st, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	preset := "{}"
	if codec := os.Getenv("STUDIOLINK_BENCH_CODEC"); codec != "" {
		settings := store.DefaultSessionSettings()
		data, _ := json.Marshal(settings)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		m["export_codec"] = codec
		data, _ = json.Marshal(m)
		preset = string(data)
	}
	session := &store.Session{ID: "bench", Title: "Export benchmark", CreatedAt: now, PresetJSON: preset}
	take := &store.Take{ID: "take", SessionID: session.ID, Number: 1, StartAtHostUs: 1_000_000, Status: "stopped", WallClockStart: now}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateSession(session))
	must(st.CreateTake(take))
	raw := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "raw")
	must(os.MkdirAll(raw, 0o700))
	writeSidecar := func(id string, extra map[string]any) {
		side := map[string]any{"trackId": id, "clockFit": align.ClockFitData{A: 1_000_000 + 0.25e6, B: 1.0000125, R2: 1, RttP10Ms: 1}, "events": []align.SyncEvent{{Type: "recorder-start", PerfTime: 0}}}
		for k, v := range extra {
			side[k] = v
		}
		data, _ := json.Marshal(side)
		must(os.WriteFile(filepath.Join(raw, id+".sidecar.json"), data, 0o600))
	}
	for i := 0; i < sources; i++ {
		pid := fmt.Sprintf("p%d", i)
		must(st.CreateParticipant(&store.Participant{ID: pid, SessionID: session.ID, Name: fmt.Sprintf("Guest %d", i), Role: "guest", Status: "admitted", JoinedAt: now}))
		must(st.CreateSource(&store.Source{ID: pid + "-cam", ParticipantID: pid, Kind: "camera", Label: "Camera"}))
		vid := pid + "-video"
		must(st.CreateTrack(&store.Track{ID: vid, TakeID: take.ID, SourceID: pid + "-cam", Kind: "video", Mime: "video/mp4", Status: "assembled"}))
		benchLink(t, camera, filepath.Join(raw, vid+".mp4"))
		writeSidecar(vid, nil)
		if pcm {
			must(st.CreateSource(&store.Source{ID: pid + "-mic", ParticipantID: pid, Kind: "mic", Label: "Microphone"}))
			aid := pid + "-pcm"
			must(st.CreateTrack(&store.Track{ID: aid, TakeID: take.ID, SourceID: pid + "-mic", Kind: "audio", Mime: "audio/wav", Status: "assembled"}))
			frames := benchPCMWAV(t, filepath.Join(raw, aid+".wav"), minutes*60)
			complete := true
			writeSidecar(aid, map[string]any{"captureComplete": &complete, "replacesAudioOfTrackId": vid, "pcm": map[string]any{"format": "float32le", "sampleRate": 48000, "channels": 1, "frames": frames}})
		}
	}
	if screen {
		must(st.CreateSource(&store.Source{ID: "p0-screen", ParticipantID: "p0", Kind: "screen", Label: "Screen"}))
		must(st.CreateTrack(&store.Track{ID: "p0-screen-video", TakeID: take.ID, SourceID: "p0-screen", Kind: "video", Mime: "video/mp4", Status: "assembled"}))
		benchLink(t, camera, filepath.Join(raw, "p0-screen-video.mp4"))
		writeSidecar("p0-screen-video", nil)
	}

	pipe := NewPipeline(st, &config.Config{DataDir: root}, runner)
	benchConfigure(t, pipe)
	start := time.Now()
	if err := pipe.ProcessTake(take.ID); err != nil {
		t.Fatal(err)
	}
	exportSec := time.Since(start).Seconds()
	status, _ := pipe.ExportStatus(take.ID)
	zipStart := time.Now()
	zipPath, err := pipe.PrepareDownload(take.ID)
	if err != nil {
		t.Fatal(err)
	}
	zipSec := time.Since(zipStart).Seconds()
	zipInfo, _ := os.Stat(zipPath)
	_ = os.Remove(zipPath)

	// Every deliverable must decode to the take length.
	out := filepath.Join(root, "sessions", session.ID, "takes", take.ID, "out")
	var outBytes int64
	for _, dir := range []string{"video", "audio"} {
		entries, err := os.ReadDir(filepath.Join(out, dir))
		must(err)
		for _, e := range entries {
			path := filepath.Join(out, dir, e.Name())
			info, _ := os.Stat(path)
			outBytes += info.Size()
			d, err := runner.ProbeDurationSec(path)
			must(err)
			if math.Abs(d-minutes*60) > 1.5 {
				t.Errorf("%s lasts %.2fs, want %.0fs", e.Name(), d, minutes*60)
			}
		}
	}
	mediaSec := minutes * 60
	report := map[string]any{
		"sources": sources, "minutes": minutes, "size": size, "pcm": pcm, "screen": screen,
		"synthSec": round2(synthSec), "exportSec": round2(exportSec), "zipSec": round2(zipSec),
		"zipMB": zipInfo.Size() / 1e6, "outputMB": outBytes / 1e6,
		"realtimeFactor": round2(mediaSec / exportSec), "status": status,
	}
	t.Logf("export %.1fs for %d × %.1f min (%.2f× realtime per take); ZIP %.1fs (%d MB); video workers %d, codec %s",
		exportSec, sources, minutes, mediaSec/exportSec, zipSec, zipInfo.Size()/1e6, status.VideoWorkers, status.VideoCodec)
	var b strings.Builder
	fmt.Fprintf(&b, "\n| stage | wall s (summed over jobs) | jobs |\n|---|---:|---:|\n")
	for _, s := range status.Stages {
		fmt.Fprintf(&b, "| %s | %.2f | %d |\n", s.Stage, s.Seconds, s.Jobs)
	}
	fmt.Fprintf(&b, "| **export total (elapsed)** | **%.2f** | |\n| zip (download prep) | %.2f | 1 |\n", exportSec, zipSec)
	t.Log(b.String())
	if path := os.Getenv("STUDIOLINK_BENCH_OUT"); path != "" {
		data, _ := json.MarshalIndent(report, "", "  ")
		must(os.WriteFile(path, data, 0o644))
	}
}

func benchEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func benchInt(t *testing.T, key string, def int) int {
	v, err := strconv.Atoi(benchEnv(key, strconv.Itoa(def)))
	if err != nil || v <= 0 {
		t.Fatalf("%s must be a positive integer", key)
	}
	return v
}

func benchFloat(t *testing.T, key string, def float64) float64 {
	v, err := strconv.ParseFloat(benchEnv(key, strconv.FormatFloat(def, 'f', -1, 64)), 64)
	if err != nil || v <= 0 {
		t.Fatalf("%s must be positive", key)
	}
	return v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// benchConfigure applies pipeline overrides from the environment.
func benchConfigure(t *testing.T, p *Pipeline) {
	if os.Getenv("STUDIOLINK_BENCH_VIDEO_WORKERS") != "" {
		p.VideoWorkers = benchInt(t, "STUDIOLINK_BENCH_VIDEO_WORKERS", 1)
	}
}

// benchCamera synthesizes (or reuses) the MediaRecorder-like camera track.
func benchCamera(t *testing.T, runner *ffmpeg.Runner, cache, size string, minutes float64) string {
	t.Helper()
	segSec := math.Min(60, minutes*60)
	seg := filepath.Join(cache, fmt.Sprintf("segment_%s_%gs.mp4", size, segSec))
	if _, err := os.Stat(seg); err != nil {
		// testsrc2 changes every frame; ±4 ms of timestamp jitter around 20 fps
		// mimics MediaRecorder's capture-time stamps.
		args := []string{"-y",
			"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=20", size),
			"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
			"-t", strconv.FormatFloat(segSec, 'f', 3, 64),
			"-vf", "setpts='(N/20+(random(1)-0.5)*0.008)/TB'", "-fps_mode", "passthrough",
			"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-row-mt", "1", "-b:v", "800k",
			"-c:a", "libopus", "-b:a", "128k", "-ac", "2",
			"-video_track_timescale", "1000",
			"-movflags", "+frag_keyframe+empty_moov+default_base_moof", seg + ".tmp.mp4"}
		if err := runner.Execute(args); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(seg+".tmp.mp4", seg); err != nil {
			t.Fatal(err)
		}
	}
	full := filepath.Join(cache, fmt.Sprintf("camera_%s_%gmin.mp4", size, minutes))
	if _, err := os.Stat(full); err == nil {
		return full
	}
	repeats := int(math.Ceil(minutes * 60 / segSec))
	list := filepath.Join(cache, "concat.txt")
	var b strings.Builder
	for i := 0; i < repeats; i++ {
		fmt.Fprintf(&b, "file '%s'\n", filepath.Base(seg))
	}
	if err := os.WriteFile(list, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runner.Execute([]string{"-y", "-f", "concat", "-safe", "0", "-i", list, "-t", strconv.FormatFloat(minutes*60, 'f', 3, 64), "-c", "copy",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof", full + ".tmp.mp4"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(full+".tmp.mp4", full); err != nil {
		t.Fatal(err)
	}
	return full
}

func benchLink(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.Link(src, dst); err == nil {
		return
	}
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
}

// benchPCMWAV writes a mono float32 WAV in the PCM recorder's exact layout.
func benchPCMWAV(t *testing.T, path string, seconds float64) uint64 {
	t.Helper()
	const rate = 48000
	frames := uint64(seconds * rate)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	header := make([]byte, 58)
	dataBytes := uint32(frames * 4)
	copy(header, "RIFF")
	binary.LittleEndian.PutUint32(header[4:], dataBytes+50)
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 18)
	binary.LittleEndian.PutUint16(header[20:], 3)
	binary.LittleEndian.PutUint16(header[22:], 1)
	binary.LittleEndian.PutUint32(header[24:], rate)
	binary.LittleEndian.PutUint32(header[28:], rate*4)
	binary.LittleEndian.PutUint16(header[32:], 4)
	binary.LittleEndian.PutUint16(header[34:], 32)
	copy(header[38:], "fact")
	binary.LittleEndian.PutUint32(header[42:], 4)
	binary.LittleEndian.PutUint32(header[46:], uint32(frames))
	copy(header[50:], "data")
	binary.LittleEndian.PutUint32(header[54:], dataBytes)
	if _, err := f.Write(header); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, rate*4)
	for i := 0; i < rate; i++ {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(float32(0.2*math.Sin(2*math.Pi*220*float64(i)/rate))))
	}
	for written := uint64(0); written < frames; written += rate {
		n := uint64(rate)
		if frames-written < n {
			n = frames - written
		}
		if _, err := f.Write(buf[:n*4]); err != nil {
			t.Fatal(err)
		}
	}
	return frames
}
