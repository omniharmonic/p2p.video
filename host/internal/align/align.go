package align

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// Sidecar mirrors the client-generated clock sidecar file.
type Sidecar struct {
	TrackID    string       `json:"trackId"`
	Kind       string       `json:"kind"`
	SampleRate int          `json:"sampleRate,omitempty"`
	ClockFit   ClockFitData `json:"clockFit"`
	Events     []SyncEvent  `json:"events"`
}

type ClockFitData struct {
	A        float64 `json:"a"` // offset intercept (hostUs)
	B        float64 `json:"b"` // drift slope
	R2       float64 `json:"r2"`
	RttP10Ms float64 `json:"rttP10Ms"`
}

type SyncEvent struct {
	Type        string  `json:"type"`     // "first-frame", "first-sample"
	PerfTime    float64 `json:"perfTime"` // client performance.now() ms
	MediaTimeUs int64   `json:"mediaTimeUs"`
}

// AlignmentResult contains calculated offset and drift.
type AlignmentResult struct {
	Method     string  `json:"method"` // "sidecar-coarse"
	OffsetSec  float64 `json:"offsetSec"`
	DriftPpm   float64 `json:"driftPpm"`
	ResidualMs float64 `json:"residualMs"`
	Confidence float64 `json:"confidence"` // 0.0 - 1.0
}

// ComputeCoarseOffset calculates the start offset in seconds from the sidecar relative to takeStartHostUs.
func ComputeCoarseOffset(sidecar *Sidecar, takeStartHostUs int64) (float64, error) {
	if sidecar == nil || len(sidecar.Events) == 0 {
		return 0.0, fmt.Errorf("no sync events in sidecar")
	}

	fit := sidecar.ClockFit
	if fit.RttP10Ms <= 0 && fit.R2 <= 0 {
		return 0, fmt.Errorf("clock fit has no measured timing samples")
	}
	if fit.RttP10Ms < 0 {
		return 0, fmt.Errorf("invalid clock round-trip time")
	}
	if math.IsNaN(fit.A) || math.IsInf(fit.A, 0) || math.IsNaN(fit.B) || math.IsInf(fit.B, 0) || fit.B < 0.9 || fit.B > 1.1 {
		return 0, fmt.Errorf("invalid or unavailable clock fit")
	}
	firstEvent := sidecar.Events[0]
	// perfTime is in milliseconds; convert to microseconds: perfTime * 1000
	clientLocalUs := firstEvent.PerfTime * 1000.0
	if math.IsNaN(clientLocalUs) || math.IsInf(clientLocalUs, 0) {
		return 0, fmt.Errorf("invalid sync event time")
	}

	// hostUs = a + b * clientLocalUs
	b := fit.B

	hostUs := fit.A + (b * clientLocalUs)

	offsetUs := hostUs - float64(takeStartHostUs)
	offsetSec := offsetUs / 1_000_000.0

	return offsetSec, nil
}

// CrossCorrelate finds the peak correlation lag between two 1D signals within [-maxLag, maxLag].
// Uses a hierarchical coarse-to-fine search (step 16 coarse, then single-sample fine) for instant execution.
func CrossCorrelate(sig1, sig2 []float64, maxLag int) (bestLag int, maxCorr float64) {
	n1 := len(sig1)
	n2 := len(sig2)
	if n1 == 0 || n2 == 0 {
		return 0, 0
	}

	bestLag = 0
	maxCorr = -1.0

	calcCorr := func(lag int) float64 {
		var dot float64
		var norm1Sub float64
		var norm2 float64
		for i := 0; i < n1; i++ {
			j := i + lag
			if j >= 0 && j < n2 {
				dot += sig1[i] * sig2[j]
				norm1Sub += sig1[i] * sig1[i]
				norm2 += sig2[j] * sig2[j]
			}
		}
		if norm1Sub > 1e-12 && norm2 > 1e-12 {
			return dot / (math.Sqrt(norm1Sub) * math.Sqrt(norm2))
		}
		return -1.0
	}

	step := 16
	if maxLag < 32 {
		step = 1
	}

	// 1. Coarse search
	coarseLag := 0
	for lag := -maxLag; lag <= maxLag; lag += step {
		score := calcCorr(lag)
		if score > maxCorr {
			maxCorr = score
			coarseLag = lag
		}
	}

	// 2. Fine search around coarse peak
	fineStart := coarseLag - step*2
	if fineStart < -maxLag {
		fineStart = -maxLag
	}
	fineEnd := coarseLag + step*2
	if fineEnd > maxLag {
		fineEnd = maxLag
	}

	bestLag = coarseLag
	for lag := fineStart; lag <= fineEnd; lag++ {
		score := calcCorr(lag)
		if score > maxCorr {
			maxCorr = score
			bestLag = lag
		}
	}

	return bestLag, maxCorr
}

// AlignTrackWithReference returns the clock-sidecar estimate. The acoustic
// check against the host-received reference is VerifyAcoustic (acoustic.go),
// run per participant by postproc.
func AlignTrackWithReference(sidecarPath string, takeStartHostUs int64, localSamples, refSamples []float64, sampleRate int) (*AlignmentResult, error) {
	return AlignTrackWithReferenceConfig(sidecarPath, takeStartHostUs, localSamples, refSamples, sampleRate, 0.40, 500)
}

// AlignTrackWithReferenceConfig returns the clock-sidecar placement of a track
// (method sidecar-coarse). The sample arguments are unused: reference
// correlation needs the packet-arrival map, see VerifyAcoustic.
func AlignTrackWithReferenceConfig(sidecarPath string, takeStartHostUs int64, localSamples, refSamples []float64, sampleRate int, threshold float64, searchWindowMs int) (*AlignmentResult, error) {
	var sidecar Sidecar
	data, err := os.ReadFile(sidecarPath)
	if err != nil {
		return nil, fmt.Errorf("read clock sidecar: %w", err)
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return nil, fmt.Errorf("decode clock sidecar: %w", err)
	}
	offset, err := ComputeCoarseOffset(&sidecar, takeStartHostUs)
	if err != nil {
		return nil, err
	}
	// Reference samples alone carry no host-time origin; VerifyAcoustic uses
	// refrec's packet log (RTP time -> host arrival) for that.
	return &AlignmentResult{Method: "sidecar-coarse", OffsetSec: offset, DriftPpm: (sidecar.ClockFit.B - 1) * 1e6, ResidualMs: sidecar.ClockFit.RttP10Ms / 2, Confidence: math.Max(0, math.Min(1, sidecar.ClockFit.R2))}, nil
}

// ComputeRationalTime converts seconds to FCPXML/rational time notation (e.g. "1001/30000s" or "3600/30s").
func ComputeRationalTime(sec float64, fps int) string {
	if sec <= 0 {
		return "0s"
	}
	frames := int64(math.Round(sec * float64(fps)))
	return fmt.Sprintf("%d/%ds", frames, fps)
}
