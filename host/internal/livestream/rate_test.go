package livestream

import (
	"math"
	"sync"
	"testing"
	"time"
)

func near(got, want float64) bool { return math.Abs(got-want) < 0.5 }

// A steady 2 Mbit/s counter reads 2000 kbps over the window; a stall decays
// to 0 within one window instead of freezing at the last value [SL-P3-2].
func TestRateMeterSlidingWindow(t *testing.T) {
	r := newRateMeter(5 * time.Second)
	t0 := time.Unix(100, 0)
	if r.kbps(t0) != 0 {
		t.Fatal("rate without samples")
	}
	// 250 ms timeslices of 62_500 bytes = 250 kB/s = 2000 kbps.
	var total int64
	for i := 0; i <= 40; i++ {
		r.observe(t0.Add(time.Duration(i)*250*time.Millisecond), total)
		total += 62_500
	}
	now := t0.Add(10 * time.Second)
	if got := r.kbps(now); !near(got, 2000) {
		t.Fatalf("steady rate = %v, want 2000", got)
	}
	if len(r.samples) > 22 {
		t.Fatalf("window kept %d samples", len(r.samples))
	}
	// Nothing new for 2.5 s: half the window is silent.
	if got := r.kbps(now.Add(2500 * time.Millisecond)); !near(got, 1000) {
		t.Fatalf("half-stalled rate = %v, want ~1000", got)
	}
	if got := r.kbps(now.Add(6 * time.Second)); got != 0 {
		t.Fatalf("stalled rate = %v, want 0", got)
	}
	// A counter that restarts (ffmpeg relaunched) starts a new series.
	r.observe(now.Add(7*time.Second), 10)
	r.observe(now.Add(8*time.Second), 125_010)
	if got := r.kbps(now.Add(8 * time.Second)); !near(got, 1000) {
		t.Fatalf("after reset rate = %v, want 1000", got)
	}
	r.reset()
	if r.kbps(now.Add(8*time.Second)) != 0 {
		t.Fatal("reset kept samples")
	}
	// Too little history says nothing.
	r.observe(t0, 0)
	r.observe(t0.Add(100*time.Millisecond), 1_000_000)
	if r.kbps(t0.Add(100*time.Millisecond)) != 0 {
		t.Fatal("rate from 100 ms of history")
	}
}

func TestApplyProgressTotalSize(t *testing.T) {
	var p Progress
	applyProgress(&p, "total_size", "N/A")
	applyProgress(&p, "bitrate", "N/A")
	if p.TotalSizeBytes != 0 || p.BitrateKbps != 0 {
		t.Fatalf("N/A parsed as %+v", p)
	}
	applyProgress(&p, "total_size", "1048576")
	if p.TotalSizeBytes != 1048576 {
		t.Fatalf("total_size = %d", p.TotalSizeBytes)
	}
	if b, src := displayBitrate(0, 0); b != 0 || src != "" {
		t.Fatal("idle bitrate")
	}
	if b, src := displayBitrate(0, 1800); b != 1800 || src != "ingest" {
		t.Fatal("ingest fallback")
	}
	if b, src := displayBitrate(2500, 1800); b != 2500 || src != "output" {
		t.Fatal("output preferred")
	}
}

// fakeClock is a settable clock for the Manager's rate windows.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
