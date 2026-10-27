package ffmpeg

import (
	"reflect"
	"testing"
)

func TestBuildScreenVideoArgsGolden(t *testing.T) {
	r := &Runner{FFmpegPath: "ffmpeg"}
	got := r.BuildScreenVideoArgs("screen.webm", "screen.mov", 30, "00:00:00:00", 1, 1367, 769)
	want := []string{
		"-y",
		"-reinit_filter", "0",
		"-i", "screen.webm",
		"-vf", "setpts=(PTS-STARTPTS)*1.000000000000,fps=30,scale=1366:768:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=1366:768:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1",
		"-fps_mode", "cfr",
		"-c:v", "libx264",
		"-b:v", "15M",
		"-pix_fmt", "yuv420p",
		"-timecode", "00:00:00:00",
		"-an", "screen.mov",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch:\n got %q\nwant %q", got, want)
	}
	// Degenerate geometry never yields a zero-sized canvas.
	tiny := r.BuildScreenVideoArgs("in", "out", 30, "", 1, 1, 0)
	for i, a := range tiny {
		if a == "-vf" && !reflect.DeepEqual(tiny[i+1], "setpts=(PTS-STARTPTS)*1.000000000000,fps=30,scale=2:2:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=2:2:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1") {
			t.Fatalf("tiny canvas filter %q", tiny[i+1])
		}
	}
}
