package clock

import (
	"math"
	"sync"
	"time"
)

var (
	baseTime      = time.Now()
	baseWallMicro = baseTime.UnixMicro()
	baseMutex     sync.RWMutex
)

// MonotonicNowUs returns monotonic microseconds elapsed since process start.
func MonotonicNowUs() int64 {
	baseMutex.RLock()
	defer baseMutex.RUnlock()
	return time.Since(baseTime).Microseconds()
}

// HostWallClockFromMonotonic converts a monotonic microsecond timestamp back to absolute UTC time.
func HostWallClockFromMonotonic(monoUs int64) time.Time {
	baseMutex.RLock()
	defer baseMutex.RUnlock()
	return baseTime.Add(time.Duration(monoUs) * time.Microsecond)
}

// LineFit represents a linear fit: y = a + b*x.
type LineFit struct {
	A  float64 `json:"a"`  // intercept (offset)
	B  float64 `json:"b"`  // slope (drift multiplier, ~1.0)
	R2 float64 `json:"r2"` // coefficient of determination
}

// Point represents a (x, y) coordinate pair.
type Point struct {
	X float64
	Y float64
}

// FitLine fits y = a + b*x using ordinary least squares.
func FitLine(points []Point) LineFit {
	n := float64(len(points))
	if n < 2 {
		return LineFit{A: 0, B: 1.0, R2: 1.0}
	}

	var sumX, sumY, sumXY, sumXX, sumYY float64
	for _, p := range points {
		sumX += p.X
		sumY += p.Y
		sumXY += p.X * p.Y
		sumXX += p.X * p.X
		sumYY += p.Y * p.Y
	}

	denominator := (n * sumXX) - (sumX * sumX)
	if math.Abs(denominator) < 1e-12 {
		return LineFit{A: sumY / n, B: 1.0, R2: 0}
	}

	b := ((n * sumXY) - (sumX * sumY)) / denominator
	a := (sumY - (b * sumX)) / n

	// Calculate R^2
	meanY := sumY / n
	var ssTot, ssRes float64
	for _, p := range points {
		pred := a + (b * p.X)
		ssRes += (p.Y - pred) * (p.Y - pred)
		ssTot += (p.Y - meanY) * (p.Y - meanY)
	}

	r2 := 1.0
	if ssTot > 1e-12 {
		r2 = 1.0 - (ssRes / ssTot)
	}

	return LineFit{A: a, B: b, R2: r2}
}
