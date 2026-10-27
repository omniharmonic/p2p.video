package ffmpeg

import (
	"reflect"
	"strings"
	"testing"
)

func TestBuildLivestreamEncodeArgsGolden(t *testing.T) {
	got, err := BuildLivestreamEncodeArgs(Livestream720p30)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-hide_banner", "-nostdin", "-loglevel", "warning", "-nostats",
		"-progress", "pipe:3", "-stats_period", "1",
		"-fflags", "+genpts+discardcorrupt",
		"-probesize", "1000000", "-analyzeduration", "1000000",
		"-thread_queue_size", "1024",
		"-i", "pipe:0",
		"-map", "0:v:0", "-map", "0:a:0?",
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2,format=yuv420p",
		"-fps_mode", "cfr", "-r", "30",
		"-c:v", "h264_videotoolbox", "-realtime", "1", "-allow_sw", "1",
		"-profile:v", "high", "-b:v", "3000k", "-maxrate", "3000k", "-bufsize", "6000k",
		"-g", "60", "-force_key_frames", "expr:gte(t,n_forced*2)",
		"-af", "aresample=async=1000:first_pts=0",
		"-c:a", "aac", "-b:a", "160k", "-ar", "48000", "-ac", "2",
		"-flvflags", "no_duration_filesize", "-flush_packets", "1",
		"-f", "flv", "pipe:1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestOutputProtocolListed(t *testing.T) {
	noNetwork := "Supported file protocols:\nInput:\n  file\n  pipe\n  rtmp\nOutput:\n  file\n  pipe\n  tee\n"
	if outputProtocolListed(noNetwork, "rtmp") {
		t.Fatal("input-only rtmp treated as output-capable")
	}
	withNetwork := noNetwork + "  rtmp\n  rtmps\n"
	if !outputProtocolListed(withNetwork, "rtmp") || !outputProtocolListed(withNetwork, "rtmps") || outputProtocolListed(withNetwork, "rtmpt") {
		t.Fatal("output protocol detection wrong")
	}
}

func TestBuildLivestreamEncodeArgs1080pNoGPLAndNoDestinations(t *testing.T) {
	got, err := BuildLivestreamEncodeArgs(Livestream1080p30)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"scale=1920:1080:", "-b:v 4500k", "-maxrate 4500k", "-bufsize 9000k", "-g 60", "-c:a aac", "-b:a 160k", "-ar 48000", "-f flv pipe:1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	// No GPL encoders, and no destination or key can ever reach the argv.
	for _, banned := range []string{"libx264", "libfdk", "libx265", "rtmp", "tee", "://"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("%s in encoder argv: %q", banned, joined)
		}
	}
}

func TestLivestreamPresetValidation(t *testing.T) {
	if _, err := BuildLivestreamEncodeArgs(LivestreamPreset{Name: "bad"}); err == nil {
		t.Fatal("zero preset accepted")
	}
	if _, err := LivestreamPresetByName("4k60"); err == nil {
		t.Fatal("unknown preset accepted")
	}
	if p, err := LivestreamPresetByName(""); err != nil || p != Livestream720p30 {
		t.Fatal("default preset is not 720p30")
	}
}
