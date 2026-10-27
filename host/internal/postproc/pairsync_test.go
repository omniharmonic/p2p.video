package postproc

import (
	"fmt"
	"math"
	"math/rand"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
)

// speechLike is noise gated into syllable-length bursts, like a voice.
func speechLike(seconds float64, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	n := int(seconds * pairRate)
	out := make([]float64, n)
	on := false
	for i := 0; i < n; i++ {
		if i%(pairRate/8) == 0 {
			on = r.Float64() < 0.6
		}
		if on {
			out[i] = r.NormFloat64() * 0.3
		}
	}
	return out
}

// memDecoder serves windows of in-memory files (sample 0 = file time 0).
func memDecoder(files map[string][]float64) pairWindowDecoder {
	return func(path string, start, dur float64) ([]float64, error) {
		s := files[path]
		a, b := int(math.Round(start*pairRate)), int(math.Round((start+dur)*pairRate))
		if a < 0 || b > len(s) {
			return nil, fmt.Errorf("window %v+%v outside %s", start, dur, path)
		}
		return s[a:b], nil
	}
}

// The rehearsal measured the PCM ~89 ms late against its own camera audio
// although both clock offsets said they started together [SL-QA-1].
func TestPairedShiftFindsLatePCM(t *testing.T) {
	voice := speechLike(70, 1)
	const late = 0.089
	pcm := append(make([]float64, int(late*pairRate)), voice...)[:len(voice)]
	dec := memDecoder(map[string][]float64{"cam": voice, "pcm": pcm})
	res, ok := pairedShift(dec, "pcm", 0.003, 60, "cam", 0.012, 60)
	if !ok {
		t.Fatal("no pairing measured")
	}
	// The PCM belongs 89 ms earlier, less the 9 ms the clocks already disagree by.
	want := -(late - (0.012 - 0.003))
	if math.Abs(res.ShiftSec-want) > 0.5/pairRate*2 || res.Corr < 0.95 {
		t.Fatalf("shift %+.4f s corr %.2f, want %+.4f", res.ShiftSec, res.Corr, want)
	}
}

func TestPairedShiftRefusesSilenceShortOverlapAndUnrelatedAudio(t *testing.T) {
	voice := speechLike(40, 2)
	silence := make([]float64, len(voice))
	other := speechLike(40, 3)
	dec := memDecoder(map[string][]float64{"cam": voice, "silent": silence, "other": other})
	if _, ok := pairedShift(dec, "silent", 0, 30, "cam", 0, 30); ok {
		t.Fatal("silent PCM paired")
	}
	if _, ok := pairedShift(dec, "other", 0, 30, "cam", 0, 30); ok {
		t.Fatal("unrelated audio paired")
	}
	if _, ok := pairedShift(dec, "cam", 0, 9, "cam", 0, 9); ok {
		t.Fatal("overlap too short for two windows")
	}
}

func TestPairedShiftRefusesWindowsThatDisagree(t *testing.T) {
	voice := speechLike(70, 4)
	// Shifted by 40 ms in the first half and 120 ms in the second: no single offset.
	pcm := make([]float64, len(voice))
	for i := range pcm {
		d := int(0.040 * pairRate)
		if i > len(pcm)/2 {
			d = int(0.120 * pairRate)
		}
		if i-d >= 0 {
			pcm[i] = voice[i-d]
		}
	}
	dec := memDecoder(map[string][]float64{"cam": voice, "pcm": pcm})
	if res, ok := pairedShift(dec, "pcm", 0, 60, "cam", 0, 60); ok {
		t.Fatalf("inconsistent pairing accepted: %+v", res)
	}
}

// Through real ffmpeg decoding: an AAC camera track and a WAV whose content is
// 87 ms late (the order and codecs the browser produces).
func TestPairedShiftThroughFFmpeg(t *testing.T) {
	runner, err := ffmpeg.NewRunner()
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	dir := t.TempDir()
	cam, pcm := filepath.Join(dir, "cam.mp4"), filepath.Join(dir, "pcm.wav")
	src := "anoisesrc=color=pink:sample_rate=48000:duration=40:seed=7,volume=0.5,tremolo=f=3:d=1"
	for _, args := range [][]string{
		{"-v", "error", "-y", "-f", "lavfi", "-i", src, "-c:a", "aac", "-b:a", "128k", cam},
		{"-v", "error", "-y", "-f", "lavfi", "-i", src, "-af", "adelay=87,atrim=0:40", "-c:a", "pcm_f32le", pcm},
	} {
		if out, err := exec.Command(runner.FFmpegPath, args...).CombinedOutput(); err != nil {
			t.Fatalf("make fixture: %v %s", err, out)
		}
	}
	dec := func(path string, start, dur float64) ([]float64, error) {
		return runner.ExtractAudioWindow(path, start, dur, pairRate)
	}
	res, ok := pairedShift(dec, pcm, 0, 40, cam, 0, 40)
	if !ok || math.Abs(res.ShiftSec+0.087) > 0.002 {
		t.Fatalf("shift %+v ok=%v, want -0.087 s", res, ok)
	}
}
