package postproc

import (
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/refrec"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Acoustic verification against the host-received reference [SL-P0-10].
//
// For each participant whose microphone the host received during the take
// (refs/<participantID>.ogg + .pkt, written by refrec), one local recording
// is measured against it: the camera file when it carries audio (its audio
// and video share the file timeline, so a correction moves both, and the
// paired PCM microphone follows it), otherwise the participant's own audio
// track. align.VerifyAcoustic decides verified / corrected / disagrees /
// unmeasurable; only "corrected" changes an offset. Screen shares are never
// measured: their audio is not the participant's microphone.

// acousticPlan is the outcome per track ID: the check to report and the
// offset correction (seconds) to add.
type acousticPlan struct {
	check    *export.AcousticCheck
	shiftSec float64
}

// acousticOutcomes runs the verification for every participant with a
// reference. Errors never fail the export: the clock estimate stays.
func (p *Pipeline) acousticOutcomes(tk *store.Take, takeDir, rawDir string, settings *store.SessionSettings, tracks []store.Track, pairedCamera map[string]store.Track) map[string]acousticPlan {
	refsDir := filepath.Join(takeDir, "refs")
	byParticipant := map[string][]store.Track{}
	for _, tr := range tracks {
		src, err := p.Store.GetSource(tr.SourceID)
		if err != nil || src == nil || src.Kind == "screen" {
			continue
		}
		byParticipant[src.ParticipantID] = append(byParticipant[src.ParticipantID], tr)
	}
	pcmOf := map[string]string{} // camera track -> paired PCM track
	for pcmID, cam := range pairedCamera {
		pcmOf[cam.ID] = pcmID
	}
	out := map[string]acousticPlan{}
	for participantID, own := range byParticipant {
		noRef := &export.AcousticCheck{Method: "gcc-phat-refrec", Status: "no-reference"}
		oggPath := filepath.Join(refsDir, participantID+".ogg")
		pkts, err := refrec.ReadPacketLog(filepath.Join(refsDir, participantID+".pkt"))
		if _, statErr := os.Stat(oggPath); err != nil || statErr != nil || len(pkts) < 50 {
			for _, tr := range own {
				out[tr.ID] = acousticPlan{check: noRef}
			}
			continue
		}
		anchor, ok := p.acousticAnchor(own, rawDir)
		if !ok {
			for _, tr := range own {
				out[tr.ID] = acousticPlan{check: &export.AcousticCheck{Method: "gcc-phat-refrec", Status: align.AcousticUnmeasurable}}
			}
			continue
		}
		res, err := p.verifyTrack(tk, rawDir, settings, anchor, oggPath, pkts)
		if err != nil {
			slog.Warn("Acoustic verification failed; keeping the clock estimate", "trackId", anchor.ID, "err", err)
			res = align.AcousticResult{Method: "gcc-phat-refrec", Status: align.AcousticUnmeasurable}
		}
		slog.Info("Acoustic verification", "participantId", participantID, "trackId", anchor.ID, "status", res.Status,
			"errorMs", res.ErrorMs, "delayMs", res.DelayMs, "spreadMs", res.SpreadMs, "driftPpm", res.DriftPpm, "windows", fmt.Sprintf("%d/%d", res.UsedWindows, res.Windows),
			"detail", windowSummary(res.Detail))
		check := &export.AcousticCheck{
			Method: res.Method, Status: res.Status, Anchor: anchor.ID,
			ErrorMs: res.ErrorMs, DelayMs: res.DelayMs, ExpectedDelayMs: res.ExpectedDelayMs, SpreadMs: res.SpreadMs,
			DriftPpm: res.DriftPpm, UncertaintyMs: res.UncertaintyMs, AppliedMs: res.AppliedMs,
			Windows: res.Windows, UsedWindows: res.UsedWindows,
		}
		// The anchor's correction applies to tracks placed through it: the
		// anchor and, for a camera, the PCM microphone measured against it.
		// Other tracks of this person carry the check for information.
		affected := map[string]bool{anchor.ID: true}
		if pcm := pcmOf[anchor.ID]; pcm != "" {
			affected[pcm] = true
		}
		for _, tr := range own {
			plan := acousticPlan{check: check}
			if affected[tr.ID] {
				plan.shiftSec = res.AppliedMs / 1000
			} else {
				info := *check
				info.AppliedMs = 0
				plan.check = &info
			}
			out[tr.ID] = plan
		}
	}
	return out
}

// acousticAnchor picks the local recording to measure: a camera with audio,
// else an audio track.
func (p *Pipeline) acousticAnchor(own []store.Track, rawDir string) (store.Track, bool) {
	for _, tr := range own {
		if tr.Kind != "video" {
			continue
		}
		if info, err := p.FFmpeg.ProbeMedia(filepath.Join(rawDir, tr.ID+rawExt(tr.Mime))); err == nil && info.HasAudio {
			return tr, true
		}
	}
	for _, tr := range own {
		if tr.Kind == "audio" {
			return tr, true
		}
	}
	return store.Track{}, false
}

func (p *Pipeline) verifyTrack(tk *store.Take, rawDir string, settings *store.SessionSettings, tr store.Track, oggPath string, pkts []refrec.Packet) (align.AcousticResult, error) {
	raw := filepath.Join(rawDir, tr.ID+rawExt(tr.Mime))
	clock, err := align.AlignTrackWithReferenceConfig(filepath.Join(rawDir, tr.ID+".sidecar.json"), tk.StartAtHostUs, nil, nil, settings.AudioSampleRate, settings.XcorrThreshold, settings.XcorrWindowMs)
	if err != nil {
		return align.AcousticResult{}, err
	}
	info, err := p.FFmpeg.ProbeMedia(raw)
	if err != nil {
		return align.AcousticResult{}, err
	}
	am, err := align.FitArrival(pkts)
	if err != nil {
		return align.AcousticResult{}, err
	}
	refStart, refEnd := math.Inf(1), math.Inf(-1)
	for _, pk := range pkts {
		refStart = math.Min(refStart, float64(pk.ArrivalUs))
		refEnd = math.Max(refEnd, float64(pk.ArrivalUs))
	}
	cfg := align.DefaultAcousticConfig()
	in := align.AcousticInput{
		OffsetSec: clock.OffsetSec, DriftBeta: 1 + clock.DriftPpm/1e6, TakeStartUs: tk.StartAtHostUs,
		DurationSec: info.DurationSec, RttMs: 2 * clock.ResidualMs, // sidecar residual is rttP10/2
		DecodeLocal: func(start, dur float64) ([]float64, error) {
			return p.FFmpeg.ExtractAudioWindow(raw, start, dur, cfg.Rate)
		},
		Reference: func(ivs []align.HostInterval) ([][]float64, []float64, error) {
			rp := align.NewReferencePlacer(pkts, am, cfg.Rate, ivs)
			if err := p.FFmpeg.StreamAudio(oggPath, cfg.Rate, rp.Feed); err != nil {
				return nil, nil, err
			}
			out, cov := rp.Result()
			return out, cov, nil
		},
		RefStartUs: refStart, RefEndUs: refEnd,
	}
	return align.VerifyAcoustic(in, cfg), nil
}

// windowSummary is a compact log form of the per-window measurements
// (local second: delay ms / peak-to-sidelobe ratio, * = used).
func windowSummary(ws []align.AcousticWindow) string {
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		mark := ""
		if w.Used {
			mark = "*"
		}
		parts = append(parts, fmt.Sprintf("%.0fs:%.1fms/psr%.1f/cov%.2f%s", w.LocalSec, w.DelayMs, w.PSR, w.Coverage, mark))
	}
	return strings.Join(parts, " ")
}
