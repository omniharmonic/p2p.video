package postproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/config"
	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/store"
)

// Pipeline orchestrates take post-processing, CFR transcoding, alignment, and NLE timeline generation.
type Pipeline struct {
	mu         sync.Mutex
	progressMu sync.Mutex
	runs       map[string]*run
	Store      *store.Store
	Config     *config.Config
	FFmpeg     *ffmpeg.Runner
	// VideoWorkers bounds concurrent video encodes; 0 picks a default for
	// the export codec (videoWorkers).
	VideoWorkers int
}

// NewPipeline creates a post-processing pipeline instance.
func NewPipeline(st *store.Store, cfg *config.Config, ff *ffmpeg.Runner) *Pipeline {
	return &Pipeline{
		Store:  st,
		Config: cfg,
		FFmpeg: ff,
	}
}

// audioWeight is an audio job's share of progress relative to a video encode
// of the same duration (measured: ~2% of the H.264 encode time).
const audioWeight = 0.05

// videoWorkers is the measured concurrency per encoder (SL-P0-9 benchmark).
// VideoToolbox H.264 encodes one frame stream at a time in hardware, so more
// than two concurrent sessions only queue; the second overlaps the next
// source's decode, probing and audio. ProRes runs on separate engines that
// scale with parallel sessions.
func (p *Pipeline) videoWorkers(codec string) int {
	if p.VideoWorkers > 0 {
		return p.VideoWorkers
	}
	if ffmpeg.NormalizeCodec(codec) == ffmpeg.CodecProResProxy {
		return max(1, min(runtime.NumCPU()/2, 4))
	}
	return 2
}

// mediaJob is one ffmpeg deliverable. Jobs run concurrently; each fills its
// own slot so the manifest keeps track order.
type mediaJob struct {
	key    string
	video  bool
	weight float64
	run    func(ctx context.Context) error
}

// ProcessTake runs the post-processing pipeline for a given take.
func (p *Pipeline) ProcessTake(takeID string) (resultErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prog := p.beginRun(takeID)
	defer func() { prog.finish(resultErr) }()
	tk, err := p.Store.GetTake(takeID)
	if err != nil {
		return fmt.Errorf("get take %s: %w", takeID, err)
	}

	if tk.Status == "recording" {
		return fmt.Errorf("take is still recording")
	}
	defer func() {
		if resultErr != nil {
			tk.Status = "failed"
			if err := p.Store.UpdateTake(tk); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("persist failed take: %w", err))
			}
		}
	}()
	missing, err := p.Store.ListMissingTakeParticipants(takeID)
	if err != nil {
		return fmt.Errorf("check expected recorders: %w", err)
	}
	if len(missing) > 0 {
		ids := make([]string, 0, len(missing))
		for _, participant := range missing {
			ids = append(ids, participant.ID)
		}
		return fmt.Errorf("missing assembled recordings from expected participants: %s", strings.Join(ids, ", "))
	}
	if p.FFmpeg == nil {
		return fmt.Errorf("ffmpeg not available: cannot transcode take %s", takeID)
	}
	sess, err := p.Store.GetSession(tk.SessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", tk.SessionID, err)
	}

	// Load session fine-tuning settings
	settings := store.DefaultSessionSettings()
	if sess.PresetJSON != "" && sess.PresetJSON != "{}" {
		if err := json.Unmarshal([]byte(sess.PresetJSON), &settings); err != nil {
			return fmt.Errorf("decode session settings: %w", err)
		}
	}
	codec := ffmpeg.NormalizeCodec(settings.ExportCodec)

	tracks, err := p.Store.ListTracksByTake(takeID)
	if err != nil {
		return fmt.Errorf("list tracks for take %s: %w", takeID, err)
	}

	if len(tracks) == 0 {
		return fmt.Errorf("take has no tracks")
	}
	for _, tr := range tracks {
		if tr.Status != "assembled" && tr.Status != "processed" {
			return fmt.Errorf("track %s is %s, awaiting assembly", tr.ID, tr.Status)
		}
	}
	userOutDir := p.exportDir(sess, tk)
	takeDir := filepath.Join(p.Config.DataDir, "sessions", tk.SessionID, "takes", takeID)
	rawDir := filepath.Join(takeDir, "raw")
	outDir := filepath.Join(takeDir, "out")
	videoDir := filepath.Join(outDir, "video")
	audioDir := filepath.Join(outDir, "audio")

	if err := os.MkdirAll(videoDir, 0755); err != nil {
		return err
	}
	if err := os.MkdirAll(audioDir, 0755); err != nil {
		return err
	}

	replacements, err := p.isolatedPCMReplacements(tracks, rawDir)
	if err != nil {
		return err
	}
	pairedCamera := pairedCameras(tracks, replacements)

	// Independent acoustic check of every participant's placement against
	// the host-received reference (refrec) [SL-P0-10].
	var acoustic map[string]acousticPlan
	_ = prog.timed(StageProbe, func() error {
		acoustic = p.acousticOutcomes(tk, takeDir, rawDir, &settings, tracks, pairedCamera)
		return nil
	})

	// Plan every deliverable first (sequential: cheap, and validation errors
	// surface before any encode starts), then run the media jobs in parallel.
	slots := make([][]*export.ExportSource, len(tracks))
	var jobs []mediaJob
	for i, tr := range tracks {
		src, err := p.Store.GetSource(tr.SourceID)
		participantName, participantID := "Unknown", ""
		sourceLabel := tr.SourceID
		isScreen, isPhone := false, false
		if err == nil && src != nil {
			sourceLabel = src.Label
			isScreen = src.Kind == "screen"
			if part, _ := p.Store.GetParticipant(src.ParticipantID); part != nil {
				participantName, participantID, isPhone = p.sourceOwner(part)
				if isPhone {
					sourceLabel = "Phone " + sourceLabel
				}
			}
		}

		rawPath := filepath.Join(rawDir, tr.ID+rawExt(tr.Mime))
		if info, err := os.Stat(rawPath); err != nil {
			return fmt.Errorf("missing assembled track %s: %w", tr.ID, err)
		} else if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("assembled track %s is empty or not a file", tr.ID)
		}

		// Clock-only alignment is intentional: reference samples do not carry
		// a take-relative origin until the RTP timestamp map is consumed.
		sidecarPath := filepath.Join(rawDir, tr.ID+".sidecar.json")
		alignResult, err := align.AlignTrackWithReferenceConfig(sidecarPath, tk.StartAtHostUs, nil, nil, settings.AudioSampleRate, settings.XcorrThreshold, settings.XcorrWindowMs)
		if err != nil {
			return fmt.Errorf("align track %s: %w", tr.ID, err)
		}

		// One ffprobe per input: duration, streams and video geometry.
		var media ffmpeg.MediaInfo
		err = prog.timed(StageProbe, func() (e error) { media, e = p.FFmpeg.ProbeMedia(rawPath); return })
		durationSec := media.DurationSec
		if err != nil || durationSec <= 0 {
			return fmt.Errorf("failed to probe duration for %s: %w", rawPath, err)
		}

		driftBeta := 1.0
		if alignResult != nil && alignResult.DriftPpm != 0.0 {
			driftBeta = 1.0 + (alignResult.DriftPpm / 1e6)
		}

		shortID := sanitize(tr.ID)
		base := export.ExportSource{
			Participant: participantName, ParticipantID: participantID,
			DurationSec: durationSec,
			DriftPpm:    alignResult.DriftPpm,
			Method:      alignResult.Method,
			ResidualMs:  alignResult.ResidualMs,
		}
		audioJob := func(key, outPath string) mediaJob {
			args := p.FFmpeg.BuildAudioNormalizeArgs(rawPath, outPath, driftBeta)
			return mediaJob{key: key, weight: durationSec * audioWeight, run: func(ctx context.Context) error {
				return prog.timed(StageAudio, func() error {
					return p.FFmpeg.ExecuteContext(ctx, args, durationSec*driftBeta, func(f float64) { prog.jobProgress(key, f) })
				})
			}}
		}

		if camera, ok := pairedCamera[tr.ID]; ok {
			_ = prog.timed(StageProbe, func() error {
				alignResult = p.refinePairedPCM(tk, rawDir, &settings, tr, camera, alignResult, durationSec)
				base.Method, base.ResidualMs = alignResult.Method, alignResult.ResidualMs
				return nil
			})
		}
		if plan, ok := acoustic[tr.ID]; ok {
			base.Acoustic = plan.check
			if plan.shiftSec != 0 {
				corrected := *alignResult
				corrected.OffsetSec += plan.shiftSec
				corrected.Method = align.MethodAcoustic
				corrected.ResidualMs = plan.check.UncertaintyMs
				alignResult = &corrected
				base.Method, base.ResidualMs = alignResult.Method, alignResult.ResidualMs
			}
		}

		if tr.Kind != "video" {
			outAudioPath := filepath.Join(audioDir, fmt.Sprintf("%s_%s_%s.wav", sanitize(participantName), sanitize(sourceLabel), shortID))
			relAudioPath := filepath.Join("audio", filepath.Base(outAudioPath))
			s := base
			s.ID, s.Label, s.Kind = tr.ID, sourceLabel, "audio"
			s.FileRelPath, s.FileAbsPath = relAudioPath, filepath.Join(userOutDir, relAudioPath)
			s.OffsetSec = alignResult.OffsetSec
			s.MicProcessing = readMicProcessing(sidecarPath)
			slots[i] = []*export.ExportSource{&s}
			job := audioJob(tr.ID, outAudioPath)
			inner := job.run
			job.run = func(ctx context.Context) error {
				if err := inner(ctx); err != nil {
					return fmt.Errorf("failed to transcode audio %s to wav: %w", rawPath, err)
				}
				return nil
			}
			jobs = append(jobs, job)
			continue
		}

		// Extract separate audio from muxed video inputs before -an video conform
		if media.HasAudio && replacements[tr.ID] == "" {
			outExtractedAudioPath := filepath.Join(audioDir, fmt.Sprintf("%s_%s_%s_audio.wav", sanitize(participantName), sanitize(sourceLabel), shortID))
			relAudioPath := filepath.Join("audio", filepath.Base(outExtractedAudioPath))
			s := base
			s.ID, s.Label, s.Kind, s.Source = tr.ID+"-audio", sourceLabel+" Audio", "audio", exportSourceKind(isScreen, isPhone)
			s.FileRelPath, s.FileAbsPath = relAudioPath, filepath.Join(userOutDir, relAudioPath)
			// The extracted WAV starts at the first decoded sample, which can sit
			// after the container start (encoder priming) [SL-QA-1].
			var audioLead float64
			_ = prog.timed(StageProbe, func() error {
				lead, err := p.FFmpeg.AudioLeadingSec(rawPath)
				if err != nil {
					slog.Warn("Audio start not measurable; assuming the container start", "trackId", tr.ID)
				}
				audioLead = lead
				return nil
			})
			s.OffsetSec = alignResult.OffsetSec + audioLead*driftBeta
			if !isScreen {
				s.MicProcessing = readMicProcessing(sidecarPath)
			}
			slots[i] = append(slots[i], &s)
			job := audioJob(tr.ID+"-audio", outExtractedAudioPath)
			inner := job.run
			job.run = func(ctx context.Context) error {
				if err := inner(ctx); err != nil {
					return fmt.Errorf("extract muxed audio: %w", err)
				}
				return nil
			}
			jobs = append(jobs, job)
		}

		if !media.HasVideo || media.Video.Width <= 0 || media.Video.Height <= 0 {
			return fmt.Errorf("probe source video: missing video geometry")
		}
		videoInfo := media.Video
		if videoInfo.LeadingOffsetSec > durationSec {
			return fmt.Errorf("video starts beyond its container duration")
		}
		outVideoPath := filepath.Join(videoDir, fmt.Sprintf("%s_%s_%s.mov", sanitize(participantName), sanitize(sourceLabel), shortID))
		relVideoPath := filepath.Join("video", filepath.Base(outVideoPath))
		s := base
		s.ID, s.Label, s.Kind, s.Source = tr.ID, sourceLabel, "video", exportSourceKind(isScreen, isPhone)
		s.FileRelPath, s.FileAbsPath = relVideoPath, filepath.Join(userOutDir, relVideoPath)
		// CFR resets PTS; preserve the original A/V start relationship in host time.
		s.OffsetSec = alignResult.OffsetSec + videoInfo.LeadingOffsetSec*driftBeta
		slots[i] = append(slots[i], &s)

		// An already-CFR H.264 camera at the target rate is copied, not
		// re-encoded (never true for MediaRecorder's VFR output).
		streamCopy := false
		if !isScreen {
			var why string
			_ = prog.timed(StageProbe, func() error {
				streamCopy, why = p.FFmpeg.CFRCopyEligible(media, rawPath, settings.TargetFPS, driftBeta, codec)
				return nil
			})
			if streamCopy {
				prog.streamCopied()
				slog.Info("Export stream-copies an already-CFR camera", "trackId", tr.ID)
			} else if media.VideoCodec == "h264" {
				slog.Info("Export re-encodes camera", "trackId", tr.ID, "reason", why)
			}
		}
		spec := ffmpeg.ConformSpec{In: rawPath, Out: outVideoPath, FPS: settings.TargetFPS, Timecode: "00:00:00:00", DriftBeta: driftBeta, Codec: codec, Screen: isScreen, Width: videoInfo.Width, Height: videoInfo.Height}
		key := tr.ID
		jobs = append(jobs, mediaJob{key: key, video: true, weight: durationSec, run: func(ctx context.Context) error {
			progress := func(f float64) { prog.jobProgress(key, f) }
			encodeErr := prog.timed(StageVideo, func() error {
				if streamCopy {
					return p.FFmpeg.ExecuteContext(ctx, p.FFmpeg.BuildRemuxVideoArgs(rawPath, outVideoPath, spec.Timecode), durationSec, progress)
				}
				// Conforming video to configured CFR (e.g. 29.97, 24, 30, 60)
				// Screen shares go onto a fixed even canvas: windows can have odd
				// sizes and be resized mid-take.
				err := p.FFmpeg.ExecuteContext(ctx, p.FFmpeg.BuildConformArgs(spec), durationSec*driftBeta, progress)
				if err != nil && p.FFmpeg.HasVideoToolbox && ctx.Err() == nil {
					// An advertised encoder can still be unavailable (busy hardware,
					// headless host, or sandbox). Retry with a real software encode.
					software := *p.FFmpeg
					software.HasVideoToolbox = false
					slog.Warn("VideoToolbox encode failed; retrying in software", "trackId", key)
					err = software.ExecuteContext(ctx, software.BuildConformArgs(spec), durationSec*driftBeta, progress)
				}
				return err
			})
			if encodeErr != nil {
				return fmt.Errorf("transcode video %s: %w", rawPath, encodeErr)
			}
			return nil
		}})
	}

	if len(jobs) == 0 {
		return fmt.Errorf("no valid tracks could be processed for take %s", takeID)
	}
	workers := p.videoWorkers(codec)
	prog.configure(workers, codec)
	for _, job := range jobs {
		prog.addJob(job.key, job.weight)
	}
	if err := runJobs(jobs, workers, max(1, min(runtime.NumCPU()/2, 4))); err != nil {
		return err
	}

	var exportSources []export.ExportSource
	for _, slot := range slots {
		for _, s := range slot {
			exportSources = append(exportSources, *s)
		}
	}
	if len(exportSources) == 0 {
		return fmt.Errorf("no valid tracks could be processed for take %s", takeID)
	}

	// Probe each actual deliverable, including drift-corrected durations,
	// in parallel (one ffprobe per output).
	verifyErrs := make([]error, len(exportSources))
	var verify sync.WaitGroup
	for i := range exportSources {
		verify.Add(1)
		go func(i int) {
			defer verify.Done()
			s := &exportSources[i]
			var info ffmpeg.MediaInfo
			err := prog.timed(StageVerify, func() (e error) { info, e = p.FFmpeg.ProbeMedia(filepath.Join(outDir, s.FileRelPath)); return })
			if err != nil || info.DurationSec <= 0 {
				verifyErrs[i] = fmt.Errorf("invalid output media %s: duration=%f error=%v", s.ID, info.DurationSec, err)
				return
			}
			s.DurationSec = info.DurationSec
			if s.Kind == "audio" {
				s.Channels = info.AudioChannels
			}
			if s.Kind == "video" {
				if !info.HasVideo || info.Video.Width <= 0 || info.Video.Height <= 0 {
					verifyErrs[i] = fmt.Errorf("probe conformed geometry: missing video geometry")
					return
				}
				s.Width, s.Height = info.Video.Width, info.Video.Height
			}
		}(i)
	}
	verify.Wait()
	if err := errors.Join(verifyErrs...); err != nil {
		return err
	}
	prog.finalStep(0.2)
	err = prog.timed(StageTimelines, func() error {
		if err := export.WriteManifest(outDir, sess.ID, sess.Title, tk.Number, tk.WallClockStart, settings.TargetFPS, exportSources, p.manifestParticipants(sess.ID, exportSources)...); err != nil {
			return fmt.Errorf("write manifest: %w", err)
		}
		if err := export.GenerateFCPXML(outDir, sess.Title, tk.Number, settings.TargetFPS, exportSources); err != nil {
			return fmt.Errorf("write FCPXML: %w", err)
		}
		if err := export.GenerateXMEML(outDir, sess.Title, tk.Number, settings.TargetFPS, exportSources); err != nil {
			return fmt.Errorf("write XMEML: %w", err)
		}
		if err := os.WriteFile(filepath.Join(outDir, "README.txt"), []byte(editorReadme(exportSources)), 0644); err != nil {
			return fmt.Errorf("write editor instructions: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	prog.finalStep(0.4)
	if err := prog.timed(StagePublish, func() error { return publishDir(outDir, userOutDir) }); err != nil {
		return fmt.Errorf("copy export: %w", err)
	}
	tk.Status = "complete"
	if err := p.Store.UpdateTake(tk); err != nil {
		return fmt.Errorf("persist complete take: %w", err)
	}

	slog.Info("Post-processing complete for take", "takeId", takeID, "outputDir", outDir, "userExportDir", userOutDir, "videoCodec", codec, "videoWorkers", workers)
	return nil
}

// runJobs runs media jobs with at most videoLimit concurrent video encodes and
// audioLimit concurrent audio jobs. Video jobs start first (longest pole).
// The first failure cancels every other job (killing its ffmpeg) and is
// returned; later cancellation errors are not reported.
func runJobs(jobs []mediaJob, videoLimit, audioLimit int) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	videoSem := make(chan struct{}, max(1, videoLimit))
	audioSem := make(chan struct{}, max(1, audioLimit))
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	ordered := make([]mediaJob, 0, len(jobs))
	for _, job := range jobs {
		if job.video {
			ordered = append(ordered, job)
		}
	}
	for _, job := range jobs {
		if !job.video {
			ordered = append(ordered, job)
		}
	}
	for _, job := range ordered {
		sem := audioSem
		if job.video {
			sem = videoSem
		}
		wg.Add(1)
		go func(job mediaJob) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			if err := job.run(ctx); err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
			}
		}(job)
	}
	wg.Wait()
	return firstErr
}

func rawExt(mime string) string {
	switch {
	case strings.Contains(mime, "mp4"):
		return ".mp4"
	case strings.Contains(mime, "wav"):
		return ".wav"
	}
	return ".webm"
}

// exportDir is the take's published package directory.
func (p *Pipeline) exportDir(sess *store.Session, tk *store.Take) string {
	exportRoot := p.Config.ExportDir
	if exportRoot == "" {
		exportRoot = filepath.Join(p.Config.DataDir, "exports")
	}
	return filepath.Join(exportRoot, sanitize(sess.Title)+"_"+sanitize(sess.ID), fmt.Sprintf("take_%02d_%s", tk.Number, sanitize(tk.ID)))
}

// exportSourceKind tags screen-share deliverables in the manifest and editor
// XML; camera/mic sources keep the field empty (unchanged manifests).
// sourceOwner is who a recorded source belongs to in the export. A phone
// (role subsource) records under its participant: same person, extra angle
// [SL-P1-9].
func (p *Pipeline) sourceOwner(part *store.Participant) (name, id string, phone bool) {
	if part.Role == "subsource" && part.ParentParticipantID != "" {
		if parent, _ := p.Store.GetParticipant(part.ParentParticipantID); parent != nil {
			return parent.Name, parent.ID, true
		}
	}
	return part.Name, part.ID, false
}

func exportSourceKind(isScreen, isPhone bool) string {
	switch {
	case isScreen:
		return export.SourceScreen
	case isPhone:
		return export.SourcePhone
	}
	return ""
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	if s == "." || s == ".." || s == "" {
		return "untitled"
	}
	return s
}

// copyFile publishes src at dst, cloning on APFS (instant, shares blocks
// copy-on-write, so later edits of either file never affect the other).
func copyFile(src, dst string) (err error) {
	if _, statErr := os.Lstat(dst); os.IsNotExist(statErr) && cloneFile(src, dst) == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		source, target := filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())
		if entry.IsDir() {
			err = copyDir(source, target)
		} else {
			err = copyFile(source, target)
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// publishDir exposes the manifest and media together. A failed copy never
// presents a partially populated final export folder. Regeneration retains the
// entire old package beside its replacement, including any editor-added files.
func publishDir(src, dst string) error {
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".studiolink-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := copyDir(src, staging); err != nil {
		return err
	}
	if info, err := os.Lstat(dst); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("export destination is not a regular directory")
		}
		backup := dst + ".previous-" + strings.TrimPrefix(filepath.Base(staging), ".studiolink-export-")
		if _, err := os.Lstat(backup); !os.IsNotExist(err) {
			return fmt.Errorf("cannot reserve previous export location")
		}
		if err := os.Rename(dst, backup); err != nil {
			return err
		}
		if err := os.Rename(staging, dst); err != nil {
			return errors.Join(err, os.Rename(backup, dst))
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(staging, dst)
}
