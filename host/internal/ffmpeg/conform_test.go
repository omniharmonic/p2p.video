package ffmpeg

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildConformArgsGolden(t *testing.T) {
	vt := &Runner{HasVideoToolbox: true}
	sw := &Runner{}
	for _, tc := range []struct {
		name string
		r    *Runner
		spec ConformSpec
		want []string
	}{
		{"h264 videotoolbox camera", vt, ConformSpec{In: "cam.mp4", Out: "cam.mov", FPS: 30, Timecode: "00:00:00:00", DriftBeta: 1.0000125, Codec: CodecH264},
			[]string{"-y", "-i", "cam.mp4", "-vf", "setpts=(PTS-STARTPTS)*1.000012500000,fps=30", "-fps_mode", "cfr", "-c:v", "h264_videotoolbox", "-b:v", "15M", "-pix_fmt", "yuv420p", "-timecode", "00:00:00:00", "-an", "cam.mov"}},
		{"prores proxy videotoolbox camera", vt, ConformSpec{In: "cam.mp4", Out: "cam.mov", FPS: 29.97, Timecode: "00:00:00:00", DriftBeta: 1, Codec: CodecProResProxy},
			[]string{"-y", "-i", "cam.mp4", "-vf", "setpts=(PTS-STARTPTS)*1.000000000000,fps=30000/1001", "-fps_mode", "cfr", "-c:v", "prores_videotoolbox", "-profile:v", "proxy", "-timecode", "00:00:00:00", "-an", "cam.mov"}},
		{"prores proxy software (LGPL prores_ks)", sw, ConformSpec{In: "cam.mp4", Out: "cam.mov", FPS: 24, DriftBeta: 0, Codec: CodecProResProxy},
			[]string{"-y", "-i", "cam.mp4", "-vf", "setpts=(PTS-STARTPTS)*1.000000000000,fps=24", "-fps_mode", "cfr", "-c:v", "prores_ks", "-profile:v", "proxy", "-vendor", "apl0", "-pix_fmt", "yuv422p10le", "-an", "cam.mov"}},
		{"prores proxy screen canvas", vt, ConformSpec{In: "screen.webm", Out: "screen.mov", FPS: 30, Timecode: "00:00:00:00", DriftBeta: 1, Codec: CodecProResProxy, Screen: true, Width: 1367, Height: 769},
			[]string{"-y", "-reinit_filter", "0", "-i", "screen.webm", "-vf", "setpts=(PTS-STARTPTS)*1.000000000000,fps=30,scale=1366:768:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=1366:768:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1", "-fps_mode", "cfr", "-c:v", "prores_videotoolbox", "-profile:v", "proxy", "-timecode", "00:00:00:00", "-an", "screen.mov"}},
		{"unknown codec stays h264", vt, ConformSpec{In: "a", Out: "b", FPS: 60, DriftBeta: 1, Codec: "vp9"},
			[]string{"-y", "-i", "a", "-vf", "setpts=(PTS-STARTPTS)*1.000000000000,fps=60", "-fps_mode", "cfr", "-c:v", "h264_videotoolbox", "-b:v", "15M", "-pix_fmt", "yuv420p", "-an", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.BuildConformArgs(tc.spec); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("argv mismatch:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
	// The legacy builders are the H.264 conform, unchanged.
	if got, want := vt.BuildCFRVideoArgsWithDrift("i", "o", 30, "", 1), vt.BuildConformArgs(ConformSpec{In: "i", Out: "o", FPS: 30, DriftBeta: 1}); !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy camera builder diverged: %q", got)
	}
}

func TestSmallBuildersGolden(t *testing.T) {
	r := &Runner{}
	for _, tc := range []struct {
		name      string
		got, want []string
	}{
		{"remux", r.BuildRemuxVideoArgs("cam.mp4", "cam.mov", "00:00:00:00"),
			[]string{"-y", "-i", "cam.mp4", "-map", "0:v:0", "-c:v", "copy", "-timecode", "00:00:00:00", "-an", "cam.mov"}},
		{"progress", WithProgress([]string{"-y", "-i", "a", "b"}), []string{"-progress", "pipe:1", "-nostats", "-y", "-i", "a", "b"}},
		{"packet end", PacketEndArgs("in.webm"), []string{"-v", "error", "-show_entries", "packet=pts_time,duration_time", "-of", "csv=p=0", "in.webm"}},
		{"probe", ProbeMediaArgs("in.mp4"), []string{"-v", "error", "-show_entries", "format=duration,start_time:stream=codec_type,codec_name,width,height,start_time,r_frame_rate,time_base,pix_fmt,channels", "-of", "json", "in.mp4"}},
		{"packets", PacketTimestampsArgs("in.mp4"), []string{"-v", "error", "-select_streams", "v:0", "-show_entries", "packet=pts", "-of", "csv=p=0", "in.mp4"}},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("%s argv mismatch:\n got %q\nwant %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestParseProbeMedia(t *testing.T) {
	info, err := parseProbeMedia([]byte(`{"streams":[{"codec_type":"video","codec_name":"vp9","width":1920,"height":1080,"start_time":"0.200000","r_frame_rate":"20/1","time_base":"1/1000","pix_fmt":"yuv420p"},{"codec_type":"audio","codec_name":"opus","start_time":"0.000000"},{"codec_type":"video","width":2,"height":2}],"format":{"duration":"60.155","start_time":"0.000000"}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := MediaInfo{DurationSec: 60.155, HasAudio: true, HasVideo: true, Video: VideoInfo{Width: 1920, Height: 1080, LeadingOffsetSec: 0.2}, VideoCodec: "vp9", PixFmt: "yuv420p", FrameRate: "20/1", TimeBase: "1/1000"}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("got %+v", info)
	}
	if _, err := parseProbeMedia([]byte(`{"streams":[],"format":{"duration":"N/A"}}`)); err == nil {
		t.Fatal("missing duration accepted")
	}
	if info, err := parseProbeMedia([]byte(`{"streams":[{"codec_type":"audio"}],"format":{"duration":"3"}}`)); err != nil || info.HasVideo || !info.HasAudio {
		t.Fatalf("audio-only probe: %+v %v", info, err)
	}
}

func TestProgressFraction(t *testing.T) {
	for _, tc := range []struct {
		line string
		want float64
		ok   bool
	}{
		{"out_time_us=1500000", 0.5, true}, {"out_time_ms=3000000", 1, true}, {"out_time_us=9000000", 1, true},
		{"out_time_us=N/A", 0, false}, {"frame=10", 0, false}, {"progress=end", 0, false},
	} {
		got, ok := progressFraction(tc.line, 3)
		if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%q = %v,%v", tc.line, got, ok)
		}
	}
}

func TestPacketsOnGrid(t *testing.T) {
	// 30 fps in a 1/15360 time base: 512 ticks per frame, decode order.
	if ok, why := packetsOnGrid([]byte("0\n1024\n512\n1536\n2048\n"), 512, 5); !ok {
		t.Fatalf("CFR rejected: %s", why)
	}
	if ok, _ := packetsOnGrid([]byte("0\n512\n1030\n1536\n2048\n"), 512, 5); ok {
		t.Fatal("jittered frame accepted")
	}
	if ok, _ := packetsOnGrid([]byte("0\n512\n1024\n"), 512, 30); ok {
		t.Fatal("dropped frames accepted")
	}
}

// Real media: an exact-CFR H.264 file qualifies for stream copy, a
// MediaRecorder-like VFR one, drifting clocks and other codecs do not.
func TestCFRCopyEligibilityOnRealMedia(t *testing.T) {
	r, err := NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	cfr, vfr := filepath.Join(dir, "cfr.mp4"), filepath.Join(dir, "vfr.mp4")
	if err := r.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=30:duration=4", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", cfr}); err != nil {
		t.Fatal(err)
	}
	if err := r.Execute([]string{"-y", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=30:duration=4", "-vf", "setpts='(N/30+(random(1)-0.5)*0.01)/TB'", "-fps_mode", "passthrough", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-video_track_timescale", "1000", vfr}); err != nil {
		t.Fatal(err)
	}
	info, err := r.ProbeMedia(cfr)
	if err != nil {
		t.Fatal(err)
	}
	if ok, why := r.CFRCopyEligible(info, cfr, 30, 1, CodecH264); !ok {
		t.Fatalf("CFR H.264 rejected: %s", why)
	}
	for _, tc := range []struct {
		name  string
		fps   float64
		beta  float64
		codec string
	}{{"other target rate", 25, 1, CodecH264}, {"drift over half a frame", 30, 1.01, CodecH264}, {"prores export", 30, 1, CodecProResProxy}} {
		if ok, _ := r.CFRCopyEligible(info, cfr, tc.fps, tc.beta, tc.codec); ok {
			t.Errorf("%s accepted", tc.name)
		}
	}
	vinfo, err := r.ProbeMedia(vfr)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.CFRCopyEligible(vinfo, vfr, 30, 1, CodecH264); ok {
		t.Fatal("VFR recording accepted for stream copy")
	}
	// The remux output keeps every frame on the grid and starts at zero.
	out := filepath.Join(dir, "copy.mov")
	if err := r.Execute(r.BuildRemuxVideoArgs(cfr, out, "00:00:00:00")); err != nil {
		t.Fatal(err)
	}
	copied, err := r.ProbeMedia(out)
	if err != nil {
		t.Fatal(err)
	}
	if copied.HasAudio || copied.Video.LeadingOffsetSec != 0 || math.Abs(copied.DurationSec-4) > 0.05 {
		t.Fatalf("remux output %+v", copied)
	}
	if ok, why := r.CFRCopyEligible(copied, out, 30, 1, CodecH264); !ok {
		t.Fatalf("remuxed file left the CFR grid: %s", why)
	}
}

func TestExecuteContextProgressAndCancel(t *testing.T) {
	r, err := NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	var last float64
	calls := 0
	err = r.ExecuteContext(context.Background(), []string{"-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=3:sample_rate=48000", "-c:a", "pcm_s16le", filepath.Join(dir, "a.wav")}, 3, func(f float64) { last = f; calls++ })
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 || last < 0.99 {
		t.Fatalf("progress calls=%d last=%f", calls, last)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = r.ExecuteContext(ctx, []string{"-y", "-re", "-f", "lavfi", "-i", "sine=frequency=440:duration=30", "-f", "null", "-"}, 30, nil)
	if err == nil || !strings.Contains(err.Error(), "cancelled") || time.Since(start) > 5*time.Second {
		t.Fatalf("cancellation: %v after %s", err, time.Since(start))
	}
}

func TestFirstAudioFrameArgsAndParse(t *testing.T) {
	got := FirstAudioFrameArgs("a b.mp4")
	want := []string{"-v", "error", "-select_streams", "a:0", "-read_intervals", "%+#8", "-show_entries", "frame=pts_time:format=start_time", "-of", "json", "a b.mp4"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("argv %q", got)
	}
	// Chromium MP4/Opus: the first decoded frame after 80 ms of priming.
	if v, err := parseAudioLeading([]byte(`{"frames":[{"pts_time":"0.080000"}],"format":{"start_time":"0.000000"}}`)); err != nil || math.Abs(v-0.08) > 1e-9 {
		t.Fatalf("leading %v %v", v, err)
	}
	if v, err := parseAudioLeading([]byte(`{"frames":[{"pts_time":"-0.006500"}],"format":{"start_time":"-0.006500"}}`)); err != nil || v != 0 {
		t.Fatalf("webm codec delay: %v %v", v, err)
	}
	if _, err := parseAudioLeading([]byte(`{"frames":[],"format":{}}`)); err == nil {
		t.Fatal("no frame accepted")
	}
}

// Firefox's MediaRecorder WebM has no container duration [SL-P1-12]: the rest
// of the probe is kept and the duration comes from the packet scan.
func TestProbeMediaWithoutContainerDuration(t *testing.T) {
	info, err := parseProbeMedia([]byte(`{"streams":[{"codec_type":"video","codec_name":"vp8","width":1920,"height":1080,"start_time":"0.000000"},{"codec_type":"audio","codec_name":"opus","start_time":"-0.001000"}],"format":{"start_time":"-0.001000"}}`))
	if !errors.Is(err, ErrNoContainerDuration) {
		t.Fatalf("err = %v, want ErrNoContainerDuration", err)
	}
	if !info.HasVideo || !info.HasAudio || info.VideoCodec != "vp8" || info.Video.Width != 1920 || info.DurationSec != 0 {
		t.Fatalf("info = %+v", info)
	}
	if _, err := parseProbeMedia([]byte(`{"streams":[],"format":{"duration":"N/A"}}`)); !errors.Is(err, ErrNoContainerDuration) {
		t.Fatalf("N/A duration: %v", err)
	}
	if _, err := parseProbeMedia([]byte(`{"streams":[],"format":{"duration":"abc"}}`)); err == nil || errors.Is(err, ErrNoContainerDuration) {
		t.Fatalf("garbage duration must stay an error: %v", err)
	}
}

func TestParsePacketEnd(t *testing.T) {
	end, err := parsePacketEnd([]byte("0.000000,0.033000\n-0.001000,0.020000\n7.966000,0.033000\n7.990000,N/A\nN/A,0.02\n7.979000,0.020000\n"))
	if err != nil || math.Abs(end-7.999) > 1e-9 {
		t.Fatalf("end = %v, %v; want 7.999", end, err)
	}
	if _, err := parsePacketEnd([]byte("N/A,N/A\n")); err == nil {
		t.Fatal("no timed packets must fail")
	}
}
