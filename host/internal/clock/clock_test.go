package clock

import (
	"math"
	"testing"
	"time"
)

func TestMonotonicClock(t *testing.T) {
	t1 := MonotonicNowUs()
	time.Sleep(10 * time.Millisecond)
	t2 := MonotonicNowUs()

	if t2 <= t1 {
		t.Fatalf("Clock failed to advance: t1=%d, t2=%d", t1, t2)
	}

	wall := HostWallClockFromMonotonic(t1)
	if math.Abs(float64(time.Since(wall).Milliseconds())) > 1000 {
		t.Errorf("Wall clock conversion discrepancy too high: %v", wall)
	}
}

func TestLineFit(t *testing.T) {
	// y = 500 + 1.00004 * x
	points := []Point{
		{X: 0, Y: 500},
		{X: 1000, Y: 1500.04},
		{X: 2000, Y: 2500.08},
		{X: 3000, Y: 3500.12},
	}

	fit := FitLine(points)
	if math.Abs(fit.A-500.0) > 0.01 {
		t.Errorf("Expected intercept ~500.0, got %f", fit.A)
	}
	if math.Abs(fit.B-1.00004) > 0.00001 {
		t.Errorf("Expected slope ~1.00004, got %f", fit.B)
	}
	if fit.R2 < 0.999 {
		t.Errorf("Expected R2 close to 1.0, got %f", fit.R2)
	}
}
