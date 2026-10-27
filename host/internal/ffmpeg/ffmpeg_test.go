package ffmpeg

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildCFRVideoArgs(t *testing.T) {
	r := &Runner{
		FFmpegPath:      "ffmpeg",
		HasVideoToolbox: true,
	}

	args := r.BuildCFRVideoArgs("input.webm", "output.mov", 30, "01:00:00:00")
	if len(args) == 0 {
		t.Fatal("Empty args")
	}

	hasCFR := false
	hasEncoder := false
	for i, a := range args {
		if a == "-fps_mode" && i+1 < len(args) && args[i+1] == "cfr" {
			hasCFR = true
		}
		if a == "-c:v" && i+1 < len(args) && args[i+1] == "h264_videotoolbox" {
			hasEncoder = true
		}
	}

	if !hasCFR {
		t.Errorf("Missing -fps_mode cfr: %v", args)
	}
	if !hasEncoder {
		t.Errorf("Missing h264_videotoolbox: %v", args)
	}
}

func TestBuildAudioWindowArgsGolden(t *testing.T) {
	got := BuildAudioWindowArgs("in put.mp4", 12.5, 8, 8000)
	want := []string{"-v", "error", "-nostdin", "-ss", "12.500000", "-i", "in put.mp4", "-t", "8.000000", "-vn", "-ac", "1", "-ar", "8000", "-f", "f32le", "pipe:1"}
	if fmt.Sprint(got) != fmt.Sprint(want) || len(got) != len(want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
}

func TestBuildAudioDecodeArgsGolden(t *testing.T) {
	got := BuildAudioDecodeArgs("refs/p 1.ogg", 16000)
	want := []string{"-v", "error", "-nostdin", "-i", "refs/p 1.ogg", "-vn", "-ac", "1", "-ar", "16000", "-f", "f32le", "pipe:1"}
	if fmt.Sprint(got) != fmt.Sprint(want) || len(got) != len(want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
}

func TestBuildAudioNormalizeArgs(t *testing.T) {
	r := &Runner{FFmpegPath: "ffmpeg"}
	got := r.BuildAudioNormalizeArgs("in put.webm", "output.wav", 1.0)
	want := []string{"-y", "-i", "in put.webm", "-vn", "-af", "aresample=async=1:min_hard_comp=0.015,aresample=48000", "-c:a", "pcm_s24le", "output.wav"}
	if fmt.Sprint(got) != fmt.Sprint(want) || len(got) != len(want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}
	drift := r.BuildAudioNormalizeArgs("in.mp4", "out.wav", 1.0001)
	if f := drift[5]; !strings.HasPrefix(f, TimestampGapFill+",aresample=") || !strings.Contains(f, ",asetrate=") {
		t.Fatalf("drift filter must fill timestamp gaps before relabelling the rate: %q", f)
	}
}

// A timestamp gap in a recording (Chromium's MediaRecorder writes one when the
// capture clock jumps under load) must stay a gap in the normalized WAV:
// closing it up moved everything after it earlier than its video [SL-QA-2].
func TestAudioNormalizeKeepsTimestampGaps(t *testing.T) {
	r, err := NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	input := filepath.Join(root, "gap.mkv")
	// 2 s of tone whose timestamps jump by 30 ms at 1 s (no samples missing).
	if err = r.Execute([]string{"-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2:sample_rate=48000", "-af", "asetnsamples=n=480,asetpts=if(gte(N\\,48000)\\,PTS+1440\\,PTS)", "-c:a", "pcm_s16le", input}); err != nil {
		t.Fatal(err)
	}
	for _, beta := range []float64{1, 1.0001} {
		output := filepath.Join(root, fmt.Sprintf("out-%v.wav", beta))
		if err = r.Execute(r.BuildAudioNormalizeArgs(input, output, beta)); err != nil {
			t.Fatal(err)
		}
		duration, err := r.ProbeDurationSec(output)
		if err != nil {
			t.Fatal(err)
		}
		if want := 2.030 * beta; math.Abs(duration-want) > 0.002 {
			t.Fatalf("beta %v: duration %.4f s; want %.4f s (the 30 ms gap kept)", beta, duration, want)
		}
		samples, err := r.ExtractAudioSamples(output, 48000, 0)
		if err != nil {
			t.Fatal(err)
		}
		// Silence where the timestamps jump, tone on both sides.
		rms := func(from, to float64) float64 {
			var s float64
			a, b := int(from*48000*beta), int(to*48000*beta)
			for _, v := range samples[a:b] {
				s += v * v
			}
			return math.Sqrt(s / float64(b-a))
		}
		if gap, before, after := rms(1.005, 1.025), rms(0.9, 0.99), rms(1.04, 1.2); gap > 0.01 || before < 0.05 || after < 0.05 {
			t.Fatalf("beta %v: rms gap %.4f before %.4f after %.4f", beta, gap, before, after)
		}
	}
}

// Drift correction must undo the device clock error transparently: stretch
// the duration by B and resample (time and pitch scale together), without
// the waveform damage a tempo stretch causes.
func TestAudioDriftResamplesTransparently(t *testing.T) {
	r, err := NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	root := t.TempDir()
	input := filepath.Join(root, "input.wav")
	output := filepath.Join(root, "output.wav")
	const beta = 1.0001 // 100 ppm, beyond typical device drift
	if err = r.Execute([]string{"-y", "-f", "lavfi", "-i", "sine=frequency=880:duration=30:sample_rate=48000", "-c:a", "pcm_f32le", input}); err != nil {
		t.Fatal(err)
	}
	if err = r.Execute(r.BuildAudioNormalizeArgs(input, output, beta)); err != nil {
		t.Fatal(err)
	}
	duration, err := r.ProbeDurationSec(output)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(duration-30*beta) > 0.002 {
		t.Fatalf("host duration=%f; want %f", duration, 30*beta)
	}
	samples, err := r.ExtractAudioSamples(output, 48000, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Compare against the ideal tone the device would have captured on a
	// correct clock: frequency 880/B, amplitude from lavfi's sine (1/8).
	f := 880 / beta
	lo, hi := 48000, len(samples)-48000
	best := 0.0
	{
		phase := 0.0
		var sig, noise float64
		// Fit amplitude/phase by least squares against sin and cos.
		var ss, sc, cc, ys, yc float64
		for n := lo; n < hi; n++ {
			w := 2 * math.Pi * f * float64(n) / 48000
			s, c := math.Sin(w+phase), math.Cos(w+phase)
			ss += s * s
			cc += c * c
			sc += s * c
			ys += samples[n] * s
			yc += samples[n] * c
		}
		det := ss*cc - sc*sc
		a := (ys*cc - yc*sc) / det
		b := (yc*ss - ys*sc) / det
		for n := lo; n < hi; n++ {
			w := 2 * math.Pi * f * float64(n) / 48000
			model := a*math.Sin(w) + b*math.Cos(w)
			sig += model * model
			d := samples[n] - model
			noise += d * d
		}
		best = 10 * math.Log10(sig/noise)
	}
	if best < 60 {
		t.Fatalf("drift-corrected tone SNR %.1f dB; want >= 60 dB", best)
	}
	t.Logf("drift-corrected tone SNR %.1f dB", best)
}

func TestIntegerFrameRatesDoNotBecomeNTSC(t *testing.T) {
	r := &Runner{}
	for _, fps := range []float64{24, 30, 60} {
		args := r.BuildCFRVideoArgs("in", "out", fps, "")
		found := false
		for i, arg := range args {
			if arg == "-vf" {
				found = true
				if args[i+1] != fmt.Sprintf("setpts=(PTS-STARTPTS)*1.000000000000,fps=%d", int(fps)) {
					t.Fatalf("%f fps became %s", fps, args[i+1])
				}
			}
		}
		if !found {
			t.Fatal("missing video filter")
		}
	}
}
