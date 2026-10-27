package align

import (
	"math"
	"math/rand"
	"testing"

	"github.com/omniharmonic/studiolink/host/internal/refrec"
)

// Synthetic-signal tests for the refrec acoustic verification [SL-P0-10]:
// a speech-like source is "captured" once, recorded locally with a known
// clock error and audio-clock drift, and sent as 20 ms Opus-sized packets
// whose host arrival times carry a known send-path delay plus jitter, loss
// and a reconnect (new SSRC and RTP base).

// speechSource is band-limited noise in syllable-like bursts on the host
// timeline (µs), linearly interpolated from a 48 kHz table.
type speechSource struct {
	t0  float64
	tab []float64
}

func newSpeechSource(seed int64, t0Us, durSec float64) *speechSource {
	r := rand.New(rand.NewSource(seed))
	n := int(durSec * 48000)
	tab := make([]float64, n)
	var lp1, lp2, hp float64
	env, target := 0.0, 0.0
	next := 0
	for i := range tab {
		if i >= next { // a new syllable or pause every 80-300 ms
			if r.Float64() < 0.65 {
				target = 0.3 + 0.7*r.Float64()
			} else {
				target = 0
			}
			next = i + int((0.08+0.22*r.Float64())*48000)
		}
		env += (target - env) * 0.002
		w := r.NormFloat64()
		lp1 += (w - lp1) * 0.35 // ~3.5 kHz low-pass
		lp2 += (lp1 - lp2) * 0.35
		hp += (lp2 - hp) * 0.01 // ~80 Hz high-pass
		tab[i] = (lp2 - hp) * env
	}
	return &speechSource{t0: t0Us, tab: tab}
}

func (s *speechSource) at(hostUs float64) float64 {
	p := (hostUs - s.t0) * 48000 / 1e6
	i := int(math.Floor(p))
	if i < 0 || i+1 >= len(s.tab) {
		return 0
	}
	f := p - float64(i)
	return s.tab[i]*(1-f) + s.tab[i+1]*f
}

type refScenario struct {
	sendDelayMs float64 // true send path + one-way delay
	jitterMs    float64 // mean of exponential arrival jitter
	lossRate    float64
	reconnectAt float64 // seconds into the reference; 0 = none
	senderPpm   float64 // sender audio clock vs host (RTP rate)
}

// buildReference returns the packet log and the decoded (pre-skip removed)
// 16 kHz stream of the reference for content captured in [startUs, endUs).
func buildReference(src *speechSource, sc refScenario, startUs, endUs float64, seed int64) ([]refrec.Packet, []float32) {
	r := rand.New(rand.NewSource(seed))
	var pkts []refrec.Packet
	type span struct{ hostUs float64 }
	var content []span                                      // capture host time of each packet's first sample
	ssrc, base := uint32(0xA1), uint32(0xFFFFFFFF-48000*20) // wraps after 20 s
	rtp := base
	rate := 48000 * (1 + sc.senderPpm/1e6) // RTP samples per host second
	reconnected := false
	for h := startUs; h < endUs; h += 960 / rate * 1e6 {
		if sc.reconnectAt > 0 && !reconnected && h-startUs >= sc.reconnectAt*1e6 {
			reconnected = true
			h += 1e6 // one second with no packets while the call reconnects
			ssrc, base = 0xB2, 0x1234
			rtp = base
		}
		ts := rtp
		rtp += 960
		if r.Float64() < sc.lossRate {
			continue
		}
		arrival := h + sc.sendDelayMs*1000 + r.ExpFloat64()*sc.jitterMs*1000
		pkts = append(pkts, refrec.Packet{SSRC: ssrc, RTPTime: ts, ArrivalUs: int64(arrival), Samples: 960})
		content = append(content, span{h})
	}
	// Decoded stream: stream sample s (48 kHz) belongs to packet s/960;
	// decoded 16 kHz sample k is stream sample 3k + pre-skip.
	total := len(pkts)*960 - refrec.OggPreSkip
	dec := make([]float32, total/3)
	for k := range dec {
		s := 3*k + refrec.OggPreSkip
		j := s / 960
		dec[k] = float32(src.at(content[j].hostUs + float64(s%960)/rate*1e6))
	}
	return pkts, dec
}

type localScenario struct {
	trueOffsetSec float64
	clockErrorMs  float64 // estimate error: content really sits this much later
	audioPpm      float64 // local audio clock drift the clock fit missed
	bleed         *speechSource
}

func runVerify(t *testing.T, src *speechSource, sc refScenario, ls localScenario, cfg AcousticConfig, durSec float64) AcousticResult {
	t.Helper()
	const takeStart = 50_000_000.0
	betaEst := 1.00002
	betaTrue := betaEst * (1 + ls.audioPpm/1e6)
	trueHost := func(tl float64) float64 { return takeStart + (ls.trueOffsetSec+tl*betaTrue)*1e6 }
	refStart, refEnd := takeStart-1e6, takeStart+(durSec+2)*1e6
	pkts, dec := buildReference(src, sc, refStart, refEnd, 7)
	am, err := FitArrival(pkts)
	if err != nil {
		t.Fatal(err)
	}
	in := AcousticInput{
		OffsetSec: ls.trueOffsetSec - ls.clockErrorMs/1000, DriftBeta: betaEst,
		TakeStartUs: int64(takeStart), DurationSec: durSec, RttMs: 2,
		DecodeLocal: func(start, dur float64) ([]float64, error) {
			x := make([]float64, int(dur*float64(cfg.Rate)))
			for i := range x {
				h := trueHost(start + float64(i)/float64(cfg.Rate))
				x[i] = src.at(h)
				if ls.bleed != nil { // a guest's voice from the speakers, only in the raw local mic
					x[i] += 0.5 * ls.bleed.at(h-40_000)
				}
			}
			return x, nil
		},
		Reference: func(ivs []HostInterval) ([][]float64, []float64, error) {
			off := 0
			next := func() ([]float32, bool) {
				if off >= len(dec) {
					return nil, false
				}
				end := min(off+4096, len(dec))
				b := dec[off:end]
				off = end
				return b, true
			}
			out, cov := ReferenceWindows(next, pkts, am, cfg.Rate, ivs)
			return out, cov, nil
		},
		RefStartUs: refStart, RefEndUs: refEnd,
	}
	return VerifyAcoustic(in, cfg)
}

func testCfg(sendPathMs float64) AcousticConfig {
	c := DefaultAcousticConfig()
	c.SendPathMs = sendPathMs
	c.MaxWindows = 8
	return c
}

func TestAcousticVerifiesAccurateClock(t *testing.T) {
	src := newSpeechSource(1, 40_000_000, 200)
	// True path 33 ms = modelled 32 ms send path + rtt/2 (1 ms).
	res := runVerify(t, src, refScenario{sendDelayMs: 33, jitterMs: 4, lossRate: 0.02}, localScenario{trueOffsetSec: 0.4}, testCfg(32), 120)
	t.Logf("%+v", res)
	if res.Status != AcousticVerified || math.Abs(res.ErrorMs) > 0.5 || res.UsedWindows < 6 {
		t.Fatalf("accurate clock not verified within 0.5 ms: %+v", res)
	}
	if math.Abs(res.DelayMs-33) > 0.5 || math.Abs(res.DriftPpm) > 5 {
		t.Fatalf("delay %v ms (want 33), drift %v ppm (want 0)", res.DelayMs, res.DriftPpm)
	}
}

func TestAcousticMeasuresInjectedClockErrors(t *testing.T) {
	src := newSpeechSource(2, 40_000_000, 200)
	for _, tc := range []struct {
		errMs  float64
		status string
	}{{-12, AcousticVerified}, {9, AcousticVerified}, {60, AcousticCorrected}, {-85, AcousticCorrected}, {180, AcousticCorrected}} {
		res := runVerify(t, src, refScenario{sendDelayMs: 33, jitterMs: 3}, localScenario{trueOffsetSec: 1.2, clockErrorMs: tc.errMs}, testCfg(32), 90)
		t.Logf("injected %v ms: %s error %.2f ms spread %.2f", tc.errMs, res.Status, res.ErrorMs, res.SpreadMs)
		if res.Status != tc.status || math.Abs(res.ErrorMs-tc.errMs) > 0.5 {
			t.Fatalf("injected %v ms: %+v", tc.errMs, res)
		}
		if tc.status == AcousticCorrected && res.AppliedMs != res.ErrorMs {
			t.Fatalf("correction not applied: %+v", res)
		}
		if tc.status == AcousticVerified && res.AppliedMs != 0 {
			t.Fatalf("a verified clock must not be shifted: %+v", res)
		}
	}
}

func TestAcousticBeyondCorrectionWindowKeepsClock(t *testing.T) {
	src := newSpeechSource(3, 40_000_000, 200)
	cfg := testCfg(32)
	cfg.MaxCorrectionMs = 100
	res := runVerify(t, src, refScenario{sendDelayMs: 33, jitterMs: 3}, localScenario{clockErrorMs: 150}, cfg, 90)
	if res.Status != AcousticDisagrees || res.AppliedMs != 0 || math.Abs(res.ErrorMs-150) > 0.5 {
		t.Fatalf("want disagrees without correction: %+v", res)
	}
}

func TestAcousticMeasuresAudioClockDrift(t *testing.T) {
	src := newSpeechSource(4, 40_000_000, 400)
	for _, ppm := range []float64{-150, 80} {
		res := runVerify(t, src, refScenario{sendDelayMs: 33, jitterMs: 3, senderPpm: 30}, localScenario{trueOffsetSec: 0.2, audioPpm: ppm}, testCfg(32), 300)
		t.Logf("injected %v ppm: drift %v ppm, error %v ms, spread %v", ppm, res.DriftPpm, res.ErrorMs, res.SpreadMs)
		// Content at local time t sits ppm·t later than the estimate.
		if math.Abs(res.DriftPpm-ppm) > 3 || res.Status == AcousticUnmeasurable {
			t.Fatalf("drift %v ppm: %+v", ppm, res)
		}
	}
}

func TestAcousticSurvivesReconnectLossJitterAndBleed(t *testing.T) {
	src := newSpeechSource(5, 40_000_000, 200)
	guest := newSpeechSource(6, 40_000_000, 200)
	res := runVerify(t, src, refScenario{sendDelayMs: 45, jitterMs: 15, lossRate: 0.05, reconnectAt: 50},
		localScenario{trueOffsetSec: 0.7, clockErrorMs: 30, bleed: guest}, testCfg(32), 120)
	t.Logf("%+v", res)
	// True path 45 ms vs modelled 33 ms: a 12 ms model error adds to the 30 ms clock error.
	if res.Status != AcousticCorrected || math.Abs(res.ErrorMs-42) > 1 {
		t.Fatalf("want corrected ≈42 ms: %+v", res)
	}
}

func TestAcousticSilenceIsUnmeasurable(t *testing.T) {
	silent := &speechSource{t0: 0, tab: make([]float64, 48000)}
	res := runVerify(t, silent, refScenario{sendDelayMs: 33}, localScenario{}, testCfg(32), 60)
	if res.Status != AcousticUnmeasurable || res.AppliedMs != 0 {
		t.Fatalf("silence must keep the clock: %+v", res)
	}
}

func TestAcousticUnrelatedAudioIsUnmeasurable(t *testing.T) {
	// The reference carries someone else's voice (wrong participant): no
	// consistent peak, so the clock estimate is kept.
	a, b := newSpeechSource(7, 40_000_000, 200), newSpeechSource(8, 40_000_000, 200)
	cfg := testCfg(32)
	const takeStart = 50_000_000.0
	pkts, dec := buildReference(b, refScenario{sendDelayMs: 33, jitterMs: 3}, takeStart-1e6, takeStart+92e6, 1)
	am, _ := FitArrival(pkts)
	res := VerifyAcoustic(AcousticInput{OffsetSec: 0, DriftBeta: 1, TakeStartUs: takeStart, DurationSec: 90, RttMs: 2,
		DecodeLocal: func(start, dur float64) ([]float64, error) {
			x := make([]float64, int(dur*16000))
			for i := range x {
				x[i] = a.at(takeStart + (start+float64(i)/16000)*1e6)
			}
			return x, nil
		},
		Reference: func(ivs []HostInterval) ([][]float64, []float64, error) {
			off := 0
			out, cov := ReferenceWindows(func() ([]float32, bool) {
				if off >= len(dec) {
					return nil, false
				}
				e := min(off+4096, len(dec))
				blk := dec[off:e]
				off = e
				return blk, true
			}, pkts, am, 16000, ivs)
			return out, cov, nil
		},
		RefStartUs: takeStart - 1e6, RefEndUs: takeStart + 92e6}, cfg)
	if res.Status == AcousticCorrected || res.Status == AcousticVerified {
		t.Fatalf("unrelated audio must not verify or correct: %+v", res)
	}
}

func TestFitArrivalRecoversMappingUnderJitter(t *testing.T) {
	r := rand.New(rand.NewSource(9))
	var pkts []refrec.Packet
	base := uint32(0xFFFF0000)
	for i := 0; i < 50*120; i++ {
		x := float64(i * 960)
		host := 1e9 + x/48000*(1+50e-6)*1e6 + 25_000
		pkts = append(pkts, refrec.Packet{SSRC: 1, RTPTime: base + uint32(i*960), ArrivalUs: int64(host + r.ExpFloat64()*8000), Samples: 960})
	}
	// A 3 s stall (reader blocked): those packets arrive late together.
	for i := 3000; i < 3150; i++ {
		pkts[i].ArrivalUs += 400_000
	}
	am, err := FitArrival(pkts)
	if err != nil {
		t.Fatal(err)
	}
	f := am.fits[1]
	for _, x := range []float64{0, 2_000_000, 5_000_000} {
		want := 1e9 + x/48000*(1+50e-6)*1e6 + 25_000
		got := f.hostUs0 + x*f.slope
		if math.Abs(got-want) > 300 {
			t.Fatalf("host at rtp %v: got %v want %v (Δ %.0f µs)", x, got, want, got-want)
		}
	}
}

func TestGCCPHATFractionalLag(t *testing.T) {
	src := newSpeechSource(10, 0, 20)
	const rate = 16000
	x := make([]float64, 8*rate)
	for i := range x {
		x[i] = src.at(1e6 + float64(i)/rate*1e6)
	}
	for _, lagMs := range []float64{0.3, 17.81, 151.06} {
		y := make([]float64, len(x)+2*rate/4)
		for i := range y { // y[k+i] = x[i] at k = lag
			y[i] = src.at(1e6 + (float64(i)/rate-lagMs/1000)*1e6)
		}
		k, psr := GCCPHAT(x, y, rate/2, rate)
		got := k / rate * 1000
		if math.Abs(got-lagMs) > 0.1 || psr < 4 {
			t.Fatalf("lag %v ms: got %.3f ms psr %.1f", lagMs, got, psr)
		}
	}
}
