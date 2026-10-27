package align

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCrossCorrelate(t *testing.T) {
	// Synthesize a signal of 100 samples
	n := 200
	sig1 := make([]float64, n)
	for i := 0; i < n; i++ {
		sig1[i] = math.Sin(float64(i)*0.2) + math.Cos(float64(i)*0.05)
	}

	// Create delayed signal shifted by +10 samples
	delay := 10
	sig2 := make([]float64, n)
	for i := delay; i < n; i++ {
		sig2[i] = sig1[i-delay]
	}

	bestLag, score := CrossCorrelate(sig1, sig2, 30)
	if bestLag != delay {
		t.Fatalf("CrossCorrelate failed: expected lag %d, got %d (score %f)", delay, bestLag, score)
	}
	if score < 0.9 {
		t.Errorf("Expected high correlation score, got %f", score)
	}
}

func TestCoarseOffset(t *testing.T) {
	sidecar := &Sidecar{
		TrackID: "t1",
		Kind:    "video",
		ClockFit: ClockFitData{
			A:  1000000.0, // hostUs intercept
			B:  1.0,
			R2: 1.0,
		},
		Events: []SyncEvent{
			{
				Type:     "first-frame",
				PerfTime: 50.0, // 50ms = 50,000 us
			},
		},
	}

	// hostUs = 1000000 + 50000 = 1050000 us
	// takeStart = 1000000 us
	// offset = 50000 us = 0.050 s
	takeStart := int64(1000000)
	offsetSec, err := ComputeCoarseOffset(sidecar, takeStart)
	if err != nil {
		t.Fatalf("ComputeCoarseOffset failed: %v", err)
	}
	if math.Abs(offsetSec-0.050) > 1e-6 {
		t.Errorf("Unexpected offset: %f", offsetSec)
	}
}

func TestComputeRationalTime(t *testing.T) {
	r := ComputeRationalTime(1.0, 30)
	if r != "30/30s" {
		t.Errorf("Expected 30/30s, got %s", r)
	}
	r0 := ComputeRationalTime(0.0, 30)
	if r0 != "0s" {
		t.Errorf("Expected 0s, got %s", r0)
	}
}

func TestMissingSidecarDoesNotInventAlignment(t *testing.T) {
	if result, err := AlignTrackWithReference("/missing/sidecar.json", 100, nil, nil, 48000); err == nil || result != nil {
		t.Fatalf("invented alignment: %+v, %v", result, err)
	}
}

func TestAudioWithoutHostOriginCannotReplaceClockOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.json")
	sidecar := Sidecar{ClockFit: ClockFitData{A: 1_000_000, B: 1, RttP10Ms: 10}, Events: []SyncEvent{{Type: "first-sample", PerfTime: 50}}}
	data, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	signal := []float64{0, 0.5, -0.3, 1, 0.1, -0.4, 0.2, 0}
	result, err := AlignTrackWithReference(path, 1_000_000, signal, signal, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if result.Method != "sidecar-coarse" || math.Abs(result.OffsetSec-0.05) > 1e-9 {
		t.Fatalf("unanchored audio invented timing: %+v", result)
	}
}

func TestUnmeasuredDefaultClockFitRejected(t *testing.T) {
	_, err := ComputeCoarseOffset(&Sidecar{ClockFit: ClockFitData{B: 1}, Events: []SyncEvent{{PerfTime: 50}}}, 0)
	if err == nil {
		t.Fatal("unmeasured default clock fit accepted")
	}
}
