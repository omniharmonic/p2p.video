package livestream

import "time"

// RateWindow is the length of the sliding windows behind IngestKbps and
// OutputKbps [SL-P3-2]. Long enough to smooth MediaRecorder's 250 ms
// timeslices and ffmpeg's ~0.5 s progress reports, short enough that a stall
// shows within a few seconds.
const RateWindow = 5 * time.Second

// rateSample is a cumulative byte counter observed at a moment.
type rateSample struct {
	at    time.Time
	total int64
}

// rateMeter turns a cumulative byte counter into a rate over a sliding
// window. It is not safe for concurrent use (the Manager's lock guards it).
type rateMeter struct {
	window  time.Duration
	samples []rateSample
}

func newRateMeter(window time.Duration) *rateMeter {
	if window <= 0 {
		window = RateWindow
	}
	return &rateMeter{window: window}
}

// observe records the counter's value at now. A counter that went backwards
// (ffmpeg relaunched) starts a new series.
func (r *rateMeter) observe(now time.Time, total int64) {
	if n := len(r.samples); n > 0 && total < r.samples[n-1].total {
		r.samples = r.samples[:0]
	}
	r.samples = append(r.samples, rateSample{at: now, total: total})
	r.trim(now)
}

// trim keeps one sample at or before the window's start (the baseline) and
// everything after it.
func (r *rateMeter) trim(now time.Time) {
	start := now.Add(-r.window)
	drop := 0
	for drop+1 < len(r.samples) && !r.samples[drop+1].at.After(start) {
		drop++
	}
	if drop > 0 {
		r.samples = append(r.samples[:0], r.samples[drop:]...)
	}
}

// kbps is the average rate over the window ending at now, in kilobits per
// second. Time without new bytes counts: a stalled counter decays to 0
// within one window instead of freezing at its last value. Fewer than two
// samples, or less than 250 ms of history, report 0 (not enough to say).
func (r *rateMeter) kbps(now time.Time) float64 {
	r.trim(now)
	if len(r.samples) < 2 {
		return 0
	}
	// The baseline is the newest sample at or before the window's start, so
	// the span is the window (or a little more when samples are sparse).
	first, last := r.samples[0], r.samples[len(r.samples)-1]
	span := now.Sub(first.at).Seconds()
	if span < 0.25 {
		return 0
	}
	bytes := last.total - first.total
	if bytes <= 0 {
		return 0
	}
	return float64(bytes) * 8 / 1000 / span
}

// delta is how much the counter grew over the window ending at now.
func (r *rateMeter) delta(now time.Time) int64 {
	r.trim(now)
	if len(r.samples) < 2 {
		return 0
	}
	return max(0, r.samples[len(r.samples)-1].total-r.samples[0].total)
}

// reset forgets every sample.
func (r *rateMeter) reset() { r.samples = r.samples[:0] }
