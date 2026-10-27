package align

// Independent acoustic verification of cross-device offsets [SL-P0-10].
//
// The clock sidecar places every track on the host timeline. The host also
// keeps its own received copy of each participant's microphone (refrec), and
// every received packet's arrival time on the host clock. That copy is an
// independent placement of the same sound: content captured at host time T
// arrives at T + D, where D is the participant's send path (WebRTC audio
// capture buffering, the 20 ms Opus frame and encoder lookahead, pacing) plus
// the minimum one-way network delay. GCC-PHAT between the local recording
// (placed by its clock estimate) and the reference (placed by its packet
// arrival times) measures where the content really is, up to D.
//
// D cannot be observed from the host. It is modelled as a nominal send-path
// delay (measured in Chromium, SendPathMs) plus half the clock round trip.
// Its error, a few milliseconds between browsers of the same kind and more
// across browser engines, bounds what the measurement can claim: a clock
// estimate within that bound is "verified", never "corrected"; only a
// confident disagreement beyond it (and within a sane window) replaces the
// clock offset. The slope of the per-window delays is a direct measurement of
// the local recording's audio clock against the host clock.

import (
	"errors"
	"math"
	"math/cmplx"
	"sort"

	"github.com/omniharmonic/studiolink/host/internal/refrec"
)

// Acoustic verification statuses (manifest "acoustic.status").
const (
	AcousticVerified     = "verified"     // the clock estimate agrees within the model bound
	AcousticCorrected    = "corrected"    // confident disagreement within bounds: offset replaced
	AcousticDisagrees    = "disagrees"    // confident but beyond the correction window: clock kept, flagged
	AcousticUnmeasurable = "unmeasurable" // too little common speech, or inconsistent windows
)

// MethodAcoustic is the alignment method of a track whose offset came from
// the reference (status corrected).
const MethodAcoustic = "refrec-gcc-phat"

// AcousticConfig holds the verification thresholds.
type AcousticConfig struct {
	Rate        int     // analysis sample rate
	WindowSec   float64 // local window length
	SearchSec   float64 // ± search around the expected reference position
	MaxWindows  int     // windows spread over the overlap
	MinPSR      float64 // GCC-PHAT peak-to-sidelobe ratio for a window to count
	MinWindows  int     // windows that must pass
	MaxSpreadMs float64 // median absolute deviation of the detrended delays
	// SendPathMs is the nominal participant send-path delay beyond the
	// one-way network delay (see the package comment).
	SendPathMs float64
	// UncertaintyMs bounds the error of the delay model: estimates within it
	// are verified, not corrected.
	UncertaintyMs float64
	// MaxCorrectionMs bounds a correction; beyond it the measurement is
	// reported but the clock estimate is kept.
	MaxCorrectionMs float64
}

// DefaultAcousticConfig returns the thresholds documented in DECISIONS
// (2026-10-26, acoustic verification).
func DefaultAcousticConfig() AcousticConfig {
	return AcousticConfig{
		Rate: 16000, WindowSec: 8, SearchSec: 0.35, MaxWindows: 16,
		MinPSR: 4, MinWindows: 3, MaxSpreadMs: 3,
		SendPathMs: DefaultSendPathMs, UncertaintyMs: 20, MaxCorrectionMs: 250,
	}
}

// DefaultSendPathMs is the reference delay, beyond rtt/2, of a Chromium
// participant whose clock placement is right: the median of the delays
// measured in the full-episode rehearsal with fake devices on loopback
// (10.3–18.3 ms over eight people-takes, median 12.8; DECISIONS 2026-10-26). It folds in
// whatever constant bias Chromium's recorder anchors have, so an error near
// zero means "consistent with a healthy Chromium participant", not
// "sample-accurate against the physical capture".
const DefaultSendPathMs = 12.0

// minLocalSec skips the start of a local file: Chromium MP4/Opus files
// carry 80 ms of encoder priming, and a window decoded from inside it came
// out exactly 80 ms off in the rehearsal.
const minLocalSec = 1.5

// AcousticWindow is one measured window.
type AcousticWindow struct {
	LocalSec float64 `json:"localSec"` // window start in the local file
	DelayMs  float64 `json:"delayMs"`  // reference minus clock placement (raw path delay)
	PSR      float64 `json:"psr"`
	Coverage float64 `json:"coverage"` // fraction of the reference window that arrived
	Used     bool    `json:"used"`
}

// AcousticResult is the verification of one local recording.
type AcousticResult struct {
	Method string `json:"method"` // "gcc-phat-refrec"
	Status string `json:"status"`
	// ErrorMs is the estimated error of the clock placement: measured delay
	// minus the modelled send path (positive: the content is later than the
	// clock estimate placed it).
	ErrorMs float64 `json:"errorMs"`
	// DelayMs is the median measured reference delay; ExpectedDelayMs the
	// modelled one (SendPathMs + rtt/2).
	DelayMs         float64 `json:"delayMs"`
	ExpectedDelayMs float64 `json:"expectedDelayMs"`
	// SpreadMs is the median absolute deviation of the detrended per-window
	// delays (measurement noise).
	SpreadMs float64 `json:"spreadMs"`
	// DriftPpm is the residual audio-clock drift against the host after the
	// clock correction (slope of the delays); reported, not applied.
	DriftPpm      float64          `json:"driftPpm"`
	UncertaintyMs float64          `json:"uncertaintyMs"`
	AppliedMs     float64          `json:"appliedMs"`
	Windows       int              `json:"windows"`
	UsedWindows   int              `json:"usedWindows"`
	Detail        []AcousticWindow `json:"-"`
}

// segmentFit maps one SSRC's RTP time to host time.
type segmentFit struct {
	base    uint32
	hostUs0 float64 // host time of RTP sample 0 (unwrapped from base)
	slope   float64 // host µs per RTP sample
}

// ArrivalMap places reference packets on the host clock.
type ArrivalMap struct {
	fits map[uint32]*segmentFit
}

func unwrap(base, ts uint32) float64 { return float64(int32(ts - base)) }

// FitArrival fits, per SSRC, host arrival time against RTP time along the
// lower envelope of the arrival delays (the least-delayed packets of each
// 2-second bin): late packets (jitter, retransmission, a stalled reader)
// only lift the envelope, they never pull it.
func FitArrival(pkts []refrec.Packet) (*ArrivalMap, error) {
	type pt struct{ x, y float64 }
	bySSRC := map[uint32][]refrec.Packet{}
	for _, p := range pkts {
		bySSRC[p.SSRC] = append(bySSRC[p.SSRC], p)
	}
	m := &ArrivalMap{fits: map[uint32]*segmentFit{}}
	for ssrc, ps := range bySSRC {
		base := ps[0].RTPTime
		const binSamples = 2 * 48000
		bins := map[int64]pt{}
		for _, p := range ps {
			x := unwrap(base, p.RTPTime)
			y := float64(p.ArrivalUs) - x*1e6/48000
			b := int64(math.Floor(x / binSamples))
			if cur, ok := bins[b]; !ok || y < cur.y {
				bins[b] = pt{x, y}
			}
		}
		var env []pt
		for _, v := range bins {
			env = append(env, v)
		}
		sort.Slice(env, func(i, j int) bool { return env[i].x < env[j].x })
		f := &segmentFit{base: base, slope: 1e6 / 48000}
		if len(env) < 3 {
			minY := math.Inf(1)
			for _, e := range env {
				minY = math.Min(minY, e.y)
			}
			f.hostUs0 = minY
		} else {
			keep := env
			var c, s float64
			for iter := 0; iter < 3; iter++ {
				c, s = lineFit(keep)
				var next []pt
				for _, e := range env {
					if e.y-(c+s*e.x) <= 2000 { // within 2 ms above the line
						next = append(next, e)
					}
				}
				if len(next) < 3 {
					break
				}
				keep = next
			}
			f.hostUs0, f.slope = c, 1e6/48000+s
		}
		m.fits[ssrc] = f
	}
	if len(m.fits) == 0 {
		return nil, errors.New("empty reference")
	}
	return m, nil
}

func lineFit[P ~struct{ x, y float64 }](pts []P) (c, s float64) {
	n := float64(len(pts))
	var sx, sy, sxx, sxy float64
	for _, p := range pts {
		x, y := struct{ x, y float64 }(p).x, struct{ x, y float64 }(p).y
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return sy / n, 0
	}
	s = (n*sxy - sx*sy) / den
	return (sy - s*sx) / n, s
}

// HostInterval is a host-clock interval (µs) to extract from the reference.
type HostInterval struct{ StartUs, EndUs float64 }

// ReferencePlacer places a decoded reference stream (mono, rate Hz, after
// the Ogg pre-skip) on the host timeline and keeps the samples inside each
// requested interval (zero where nothing arrived). Feed it the decoded
// blocks in order.
type ReferencePlacer struct {
	pkts    []refrec.Packet
	am      *ArrivalMap
	ivs     []HostInterval
	out     [][]float64
	filled  []int
	ratio   float64
	rateUs  float64
	j, k    int
	cum, px float64
	fit     *segmentFit
}

// NewReferencePlacer prepares the intervals to extract.
func NewReferencePlacer(pkts []refrec.Packet, am *ArrivalMap, rate int, intervals []HostInterval) *ReferencePlacer {
	rp := &ReferencePlacer{pkts: pkts, am: am, ivs: intervals, ratio: 48000 / float64(rate), rateUs: float64(rate) / 1e6,
		out: make([][]float64, len(intervals)), filled: make([]int, len(intervals))}
	for i, iv := range intervals {
		rp.out[i] = make([]float64, int(math.Round((iv.EndUs-iv.StartUs)*rp.rateUs)))
	}
	rp.setPacket()
	return rp
}

func (rp *ReferencePlacer) setPacket() {
	if rp.j < len(rp.pkts) {
		rp.fit = rp.am.fits[rp.pkts[rp.j].SSRC]
		rp.px = unwrap(rp.fit.base, rp.pkts[rp.j].RTPTime)
	}
}

// Feed places the next block of decoded samples. Decoded sample k is Ogg
// stream sample k·48000/rate + OggPreSkip; the packet log says which packet
// (SSRC, RTP time) it belongs to, and the arrival fit where that is on the
// host clock.
func (rp *ReferencePlacer) Feed(block []float32) {
	for _, v := range block {
		s := float64(rp.k)*rp.ratio + refrec.OggPreSkip
		rp.k++
		for rp.j < len(rp.pkts) && s >= rp.cum+float64(rp.pkts[rp.j].Samples) {
			rp.cum += float64(rp.pkts[rp.j].Samples)
			rp.j++
			rp.setPacket()
		}
		if rp.j >= len(rp.pkts) {
			return
		}
		host := rp.fit.hostUs0 + (rp.px+s-rp.cum)*rp.fit.slope
		for i, iv := range rp.ivs {
			if host < iv.StartUs || host >= iv.EndUs {
				continue
			}
			if idx := int((host - iv.StartUs) * rp.rateUs); idx >= 0 && idx < len(rp.out[i]) {
				rp.out[i][idx] = float64(v)
				rp.filled[i]++
			}
		}
	}
}

// Result returns each interval's samples and the fraction of it covered.
func (rp *ReferencePlacer) Result() ([][]float64, []float64) {
	cov := make([]float64, len(rp.out))
	for i := range rp.out {
		if len(rp.out[i]) > 0 {
			cov[i] = math.Min(1, float64(rp.filled[i])/float64(len(rp.out[i])))
		}
	}
	return rp.out, cov
}

// ReferenceWindows runs a ReferencePlacer over blocks from next (false at
// the end of the stream).
func ReferenceWindows(next func() ([]float32, bool), pkts []refrec.Packet, am *ArrivalMap, rate int, intervals []HostInterval) ([][]float64, []float64) {
	rp := NewReferencePlacer(pkts, am, rate, intervals)
	for {
		block, ok := next()
		if !ok {
			return rp.Result()
		}
		rp.Feed(block)
	}
}

// GCCPHAT returns the lag (samples, fractional) in [0, maxK] at which x best
// matches y[k:k+len(x)], using the phase transform (whitened cross-power
// spectrum, band-limited to speech), and the peak-to-sidelobe ratio: the
// peak over the largest correlation more than 1 ms away from it.
func GCCPHAT(x, y []float64, maxK, rate int) (lag, psr float64) {
	if len(x) == 0 || len(y) < len(x) {
		return 0, 0
	}
	maxK = min(maxK, len(y)-len(x))
	n := nextPow2(len(x) + len(y))
	X := make([]complex128, n)
	Y := make([]complex128, n)
	for i, v := range x {
		X[i] = complex(v, 0)
	}
	for i, v := range y {
		Y[i] = complex(v, 0)
	}
	fft(X, false)
	fft(Y, false)
	lo := int(100 * float64(n) / float64(rate))
	hi := int(6000 * float64(n) / float64(rate))
	G := make([]complex128, n)
	for i := 0; i < n; i++ {
		f := i
		if f > n/2 {
			f = n - i
		}
		if f < lo || f > hi {
			continue
		}
		c := cmplx.Conj(X[i]) * Y[i]
		mag := cmplx.Abs(c)
		if mag > 1e-12 {
			G[i] = c / complex(mag, 0)
		}
	}
	fft(G, true)
	r := make([]float64, maxK+1)
	best := 0
	for k := 0; k <= maxK; k++ {
		r[k] = real(G[k])
		if r[k] > r[best] {
			best = k
		}
	}
	if r[best] <= 0 {
		return 0, 0
	}
	guard := rate / 1000
	side := 0.0
	for k, v := range r {
		if k < best-guard || k > best+guard {
			side = math.Max(side, math.Abs(v))
		}
	}
	lag = float64(best)
	if best > 0 && best < maxK {
		a, b, c := r[best-1], r[best], r[best+1]
		if den := a - 2*b + c; den < 0 {
			lag += 0.5 * (a - c) / den
		}
	}
	if side <= 1e-12 {
		return lag, math.Inf(1)
	}
	return lag, r[best] / side
}

// AcousticInput describes one local recording to verify.
type AcousticInput struct {
	// OffsetSec places the local file's time 0 on the take timeline;
	// DriftBeta scales local time to host time (1 + driftPpm/1e6).
	OffsetSec, DriftBeta float64
	TakeStartUs          int64
	DurationSec          float64
	RttMs                float64
	// DecodeLocal returns rate-Hz mono samples of the local file from start
	// (seconds, file time) for dur seconds.
	DecodeLocal func(start, dur float64) ([]float64, error)
	// Reference returns the reference samples (host timeline) for each
	// interval, and their coverage.
	Reference func([]HostInterval) ([][]float64, []float64, error)
	// RefSpanUs is the host interval the reference covers.
	RefStartUs, RefEndUs float64
}

// VerifyAcoustic measures the local recording against its reference and
// decides whether the clock placement is verified, corrected or kept.
func VerifyAcoustic(in AcousticInput, cfg AcousticConfig) AcousticResult {
	res := AcousticResult{Method: "gcc-phat-refrec", Status: AcousticUnmeasurable, UncertaintyMs: cfg.UncertaintyMs}
	beta := in.DriftBeta
	if beta <= 0 {
		beta = 1
	}
	expected := cfg.SendPathMs + in.RttMs/2
	res.ExpectedDelayMs = round2(expected)
	hostOf := func(t float64) float64 { // host µs of local file time t
		return float64(in.TakeStartUs) + (in.OffsetSec+t*beta)*1e6
	}
	// Local times whose reference window lies inside the reference span.
	pad := cfg.SearchSec + 0.05
	lo := math.Max(minLocalSec, (in.RefStartUs/1e6-float64(in.TakeStartUs)/1e6-in.OffsetSec)/beta+pad)
	hi := math.Min(in.DurationSec, (in.RefEndUs/1e6-float64(in.TakeStartUs)/1e6-in.OffsetSec)/beta-pad) - cfg.WindowSec
	if hi-lo < 1 {
		return res
	}
	nWin := cfg.MaxWindows
	if span := hi - lo; span < float64(nWin)*cfg.WindowSec {
		nWin = max(1, int(span/cfg.WindowSec)+1)
	}
	starts := make([]float64, nWin)
	ivs := make([]HostInterval, nWin)
	for i := range starts {
		t := lo
		if nWin > 1 {
			t = lo + (hi-lo)*float64(i)/float64(nWin-1)
		}
		starts[i] = t
		c := hostOf(t) + expected*1000
		ivs[i] = HostInterval{StartUs: c - cfg.SearchSec*1e6, EndUs: c + (cfg.WindowSec*beta+cfg.SearchSec)*1e6}
	}
	refs, cov, err := in.Reference(ivs)
	if err != nil {
		return res
	}
	res.Windows = nWin
	search := int(cfg.SearchSec * float64(cfg.Rate))
	type obs struct{ t, d float64 }
	var good []obs
	for i, t := range starts {
		w := AcousticWindow{LocalSec: round2(t), Coverage: round2(cov[i])}
		x, err := in.DecodeLocal(t, cfg.WindowSec)
		if err == nil && cov[i] >= 0.9 && energetic(x) && energetic(refs[i]) {
			k, psr := GCCPHAT(x, refs[i], 2*search, cfg.Rate)
			w.PSR = round2(psr)
			// Content at local t sits in the reference at index k, i.e. at
			// host hostOf(t) + expected - search + k: the measured delay.
			w.DelayMs = round2(expected + (k-float64(search))/float64(cfg.Rate)*1000)
			edge := k < 0.02*float64(search) || k > 1.98*float64(search)
			if psr >= cfg.MinPSR && !edge {
				w.Used = true
				good = append(good, obs{t, w.DelayMs})
			}
		}
		res.Detail = append(res.Detail, w)
	}
	res.UsedWindows = len(good)
	if len(good) < cfg.MinWindows {
		return res
	}
	// Theil–Sen slope and median intercept: robust to a stray window.
	var slopes []float64
	for i := range good {
		for j := i + 1; j < len(good); j++ {
			if dt := good[j].t - good[i].t; dt > 1 {
				slopes = append(slopes, (good[j].d-good[i].d)/dt)
			}
		}
	}
	slope := 0.0
	if len(slopes) > 0 {
		slope = median(slopes)
	}
	mid := (good[0].t + good[len(good)-1].t) / 2
	var levels, devs []float64
	for _, o := range good {
		levels = append(levels, o.d-slope*(o.t-mid))
	}
	level := median(levels)
	for _, l := range levels {
		devs = append(devs, math.Abs(l-level))
	}
	res.SpreadMs = round2(median(devs))
	res.DelayMs = round2(level)
	res.DriftPpm = round2(slope * 1000) // ms per s -> ppm
	res.ErrorMs = round2(level - expected)
	if res.SpreadMs > cfg.MaxSpreadMs || float64(len(good)) < 0.5*float64(nWin) {
		return res
	}
	switch e := math.Abs(res.ErrorMs); {
	case e <= cfg.UncertaintyMs:
		res.Status = AcousticVerified
	case e <= cfg.MaxCorrectionMs:
		res.Status = AcousticCorrected
		res.AppliedMs = res.ErrorMs
	default:
		res.Status = AcousticDisagrees
	}
	return res
}

func energetic(x []float64) bool {
	if len(x) == 0 {
		return false
	}
	var e float64
	for _, v := range x {
		e += v * v
	}
	return e/float64(len(x)) > 1e-7 // about -70 dBFS RMS
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
