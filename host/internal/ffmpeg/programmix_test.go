package ffmpeg

import (
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildProgramMixArgsGolden(t *testing.T) {
	got, err := BuildProgramMixArgs([]MixInput{
		{Path: "/pkg/audio/Ana.wav", OffsetSec: 0},
		{Path: "/pkg/audio/Ben.wav", OffsetSec: 1.2345},
		{Path: "/pkg/audio/Cy.wav", OffsetSec: -0.25},
	}, "/pkg/handoff/program.wav")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-y", "-nostdin",
		"-i", "/pkg/audio/Ana.wav", "-i", "/pkg/audio/Ben.wav", "-i", "/pkg/audio/Cy.wav",
		"-filter_complex",
		"[0:a]aresample=48000,aformat=channel_layouts=mono[m0];" +
			"[1:a]aresample=48000,aformat=channel_layouts=mono,adelay=delays=1235:all=1[m1];" +
			"[2:a]aresample=48000,aformat=channel_layouts=mono,atrim=start=0.250,asetpts=PTS-STARTPTS[m2];" +
			"[m0][m1][m2]amix=inputs=3:duration=longest:normalize=0,alimiter=limit=0.95:latency=1[mix]",
		"-map", "[mix]", "-ac", "1", "-ar", "48000", "-c:a", "pcm_s16le", "/pkg/handoff/program.wav"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv:\n%q\nwant\n%q", got, want)
	}
	if _, err := BuildProgramMixArgs(nil, "/o.wav"); err == nil {
		t.Fatal("empty input list accepted")
	}
	if _, err := BuildProgramMixArgs([]MixInput{{Path: "-i"}}, "/o.wav"); err == nil {
		t.Fatal("flag-like input accepted")
	}
}

// Synthetic-signal check: a 1 kHz burst at file t=0 delayed by 0.5 s lands at
// 0.5 s in the mix, and the mix lasts as long as the latest-ending input.
func TestProgramMixPlacesInputsOnTakeTimeline(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	tone := filepath.Join(dir, "tone.wav")
	silence := filepath.Join(dir, "silence.wav")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
		}
	}
	run("-y", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=0.2", "-c:a", "pcm_s24le", tone)
	run("-y", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=mono", "-t", "1.0", "-c:a", "pcm_s24le", silence)
	out := filepath.Join(dir, "mix.wav")
	args, err := BuildProgramMixArgs([]MixInput{{Path: silence}, {Path: tone, OffsetSec: 0.5}}, out)
	if err != nil {
		t.Fatal(err)
	}
	run(args...)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	pcm := wavData(t, data)
	n := len(pcm) / 2
	if d := float64(n) / 48000; math.Abs(d-1.0) > 0.03 {
		t.Fatalf("mix duration %.3f s, want ~1.0", d)
	}
	first := -1
	for i := 0; i < n; i++ {
		if v := int16(binary.LittleEndian.Uint16(pcm[2*i:])); v > 3000 || v < -3000 {
			first = i
			break
		}
	}
	if at := float64(first) / 48000; first < 0 || math.Abs(at-0.5) > 0.005 {
		t.Fatalf("tone onset at %.4f s, want 0.5", at)
	}
}

// wavData returns the payload of the "data" chunk (ffmpeg adds a LIST chunk).
func wavData(t *testing.T, b []byte) []byte {
	t.Helper()
	for i := 12; i+8 <= len(b); {
		id, size := string(b[i:i+4]), int(binary.LittleEndian.Uint32(b[i+4:]))
		if id == "data" {
			end := i + 8 + size
			if end > len(b) {
				end = len(b)
			}
			return b[i+8 : end]
		}
		i += 8 + size + size%2
	}
	t.Fatal("no data chunk")
	return nil
}
