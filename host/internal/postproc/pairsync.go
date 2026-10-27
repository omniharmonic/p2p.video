package postproc

import (
	"log/slog"
	"math"
	"path/filepath"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Paired PCM alignment [SL-QA-1].
//
// A participant's uncompressed PCM microphone and the audio embedded in their
// own camera recording come from the same microphone, so their content is
// identical up to a time shift. The camera file's audio and video are muxed
// from capture timestamps by MediaRecorder; the PCM track's clock anchor is a
// render-start estimate that does not include input latency (its sidecar
// says so). The full-episode rehearsal measured the PCM ~80-90 ms late against
// its own camera in Chromium, i.e. lips ahead of the voice in the editor.
// Measuring the shift acoustically against the paired camera audio removes
// that: the camera's clock offset remains the only cross-device estimate.

const (
	pairRate      = 8000 // analysis sample rate (speech band is enough)
	pairWindowSec = 8.0  // correlated span per window
	pairSearchSec = 0.5  // ± search range
	pairMinCorr   = 0.6  // normalized correlation needed in each window
	pairAgreeSec  = 0.010
)

// pairWindowDecoder decodes `dur` seconds of a file's audio from `start`.
type pairWindowDecoder func(path string, start, dur float64) ([]float64, error)

// PairResult is the measured correction for a PCM track.
type PairResult struct {
	// ShiftSec is added to the PCM track's clock offset (negative: the PCM
	// content was late against its camera).
	ShiftSec   float64
	ResidualMs float64
	Corr       float64
}

// pairedShift measures where the PCM file's content sits against its paired
// camera's embedded audio, both placed on the take timeline by their clock
// offsets. ok is false (keep the clock estimate) when the overlap is too
// short, a window is silent or ambiguous, or the two windows disagree.
func pairedShift(decode pairWindowDecoder, pcmPath string, pcmOff, pcmDur float64, camPath string, camOff, camDur float64) (PairResult, bool) {
	from := math.Max(pcmOff, camOff) + pairSearchSec
	to := math.Min(pcmOff+pcmDur, camOff+camDur) - pairSearchSec - pairWindowSec
	if to-from < 1 {
		return PairResult{}, false
	}
	var shifts, corrs []float64
	for _, frac := range []float64{0.25, 0.75} {
		t := from + (to-from)*frac
		x, err := decode(pcmPath, t-pcmOff, pairWindowSec)
		if err != nil {
			return PairResult{}, false
		}
		y, err := decode(camPath, t-camOff-pairSearchSec, pairWindowSec+2*pairSearchSec)
		if err != nil {
			return PairResult{}, false
		}
		k, c := bestContainedLag(x, y, int(2*pairSearchSec*pairRate))
		if c < pairMinCorr {
			return PairResult{}, false
		}
		// x (PCM at timeline t) matches y at index k, i.e. camera timeline
		// t - search + k/rate: that is where the PCM content really belongs.
		shifts = append(shifts, float64(k)/pairRate-pairSearchSec)
		corrs = append(corrs, c)
	}
	if math.Abs(shifts[0]-shifts[1]) > pairAgreeSec {
		return PairResult{}, false
	}
	shift := (shifts[0] + shifts[1]) / 2
	if math.Abs(shift) > pairSearchSec*0.9 {
		return PairResult{}, false // at the edge of the search: not a real peak
	}
	return PairResult{ShiftSec: shift, ResidualMs: math.Abs(shifts[0]-shifts[1]) / 2 * 1000, Corr: math.Min(corrs[0], corrs[1])}, true
}

// bestContainedLag returns k in [0, maxK] maximizing the normalized
// correlation of x against y[k:k+len(x)] (coarse step, then exact).
func bestContainedLag(x, y []float64, maxK int) (int, float64) {
	n := len(x)
	if n == 0 || len(y) < n {
		return 0, -1
	}
	if maxK > len(y)-n {
		maxK = len(y) - n
	}
	var xx float64
	for _, v := range x {
		xx += v * v
	}
	if xx < 1e-9 {
		return 0, -1
	}
	corr := func(k int) float64 {
		var dot, yy float64
		seg := y[k : k+n]
		for i, v := range x {
			dot += v * seg[i]
			yy += seg[i] * seg[i]
		}
		if yy < 1e-9 {
			return -1
		}
		return dot / math.Sqrt(xx*yy)
	}
	const step = 2
	best, bestC := 0, -2.0
	for k := 0; k <= maxK; k += step {
		if c := corr(k); c > bestC {
			best, bestC = k, c
		}
	}
	lo, hi := max(0, best-step), min(maxK, best+step)
	for k := lo; k <= hi; k++ {
		if c := corr(k); c > bestC {
			best, bestC = k, c
		}
	}
	return best, bestC
}

// refinePairedPCM returns the PCM track's clock alignment corrected against
// its paired camera (from isolatedPCMReplacements), or the clock
// alignment unchanged when it can't be measured.
func (p *Pipeline) refinePairedPCM(tk *store.Take, rawDir string, settings *store.SessionSettings, pcm store.Track, camera store.Track, clock *align.AlignmentResult, pcmDur float64) *align.AlignmentResult {
	camRaw := filepath.Join(rawDir, camera.ID+rawExt(camera.Mime))
	camAlign, err := align.AlignTrackWithReferenceConfig(filepath.Join(rawDir, camera.ID+".sidecar.json"), tk.StartAtHostUs, nil, nil, settings.AudioSampleRate, settings.XcorrThreshold, settings.XcorrWindowMs)
	if err != nil {
		return clock
	}
	camInfo, err := p.FFmpeg.ProbeMedia(camRaw)
	if err != nil || !camInfo.HasAudio {
		return clock
	}
	decode := func(path string, start, dur float64) ([]float64, error) {
		return p.FFmpeg.ExtractAudioWindow(path, start, dur, pairRate)
	}
	res, ok := pairedShift(decode, filepath.Join(rawDir, pcm.ID+rawExt(pcm.Mime)), clock.OffsetSec, pcmDur, camRaw, camAlign.OffsetSec, camInfo.DurationSec)
	if !ok {
		slog.Info("Paired PCM alignment not measurable; keeping the clock estimate", "trackId", pcm.ID)
		return clock
	}
	slog.Info("Paired PCM aligned to its camera audio", "trackId", pcm.ID, "shiftMs", math.Round(res.ShiftSec*1000*10)/10, "corr", math.Round(res.Corr*100)/100)
	out := *clock
	out.OffsetSec += res.ShiftSec
	out.Method = "paired-xcorr"
	out.ResidualMs = math.Max(res.ResidualMs, camAlign.ResidualMs)
	return &out
}

// pairedCameras maps each PCM track ID to the camera track it replaces.
func pairedCameras(tracks []store.Track, replacements map[string]string) map[string]store.Track {
	byID := make(map[string]store.Track, len(tracks))
	for _, t := range tracks {
		byID[t.ID] = t
	}
	out := make(map[string]store.Track, len(replacements))
	for cameraID, pcmID := range replacements {
		if cam, ok := byID[cameraID]; ok {
			out[pcmID] = cam
		}
	}
	return out
}
