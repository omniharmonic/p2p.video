// Package integrations runs the post-take ecosystem handoffs for an exported
// take (ROADMAP P4, SL-P4-1, SL-P4-3): a transcript built from each
// participant's isolated track by the built-in on-device engine (the Mac
// app) or Minutes, a Parachute session note, and the
// clip-editor handoff. Jobs run in the background, one of each kind per take,
// and only ever read the published export package (never call media).
package integrations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/ffmpeg"
	"github.com/omniharmonic/studiolink/host/internal/handoff"
	"github.com/omniharmonic/studiolink/host/internal/minutes"
	"github.com/omniharmonic/studiolink/host/internal/parachute"
	"github.com/omniharmonic/studiolink/host/internal/store"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

// Package gives locked access to a take's published export package.
type Package interface {
	PublishedPackageDir(takeID string) (string, error)
	WithPublishedPackage(takeID string, fn func(dir string) error) error
}

// Transcriber transcribes one isolated file (minutes.Client).
type Transcriber interface {
	Supported(ctx context.Context) error
	TranscribeFile(ctx context.Context, audioPath, speaker string, durationSec float64) (*minutes.FileTranscript, error)
}

// NoteSender saves a vault note (parachute.Client).
type NoteSender interface {
	SaveNote(ctx context.Context, note parachute.Note, previous *parachute.Receipt) (parachute.Receipt, error)
	VaultURL() string
}

// Mixer renders the reference program mix and probes durations.
type Mixer interface {
	Mix(inputs []ffmpeg.MixInput, outPath string) error
	Duration(path string) (float64, error)
}

// FFmpegMixer adapts ffmpeg.Runner.
type FFmpegMixer struct{ Runner *ffmpeg.Runner }

// Mix implements Mixer with the argv builder.
func (m FFmpegMixer) Mix(inputs []ffmpeg.MixInput, outPath string) error {
	args, err := ffmpeg.BuildProgramMixArgs(inputs, outPath)
	if err != nil {
		return err
	}
	return m.Runner.Execute(args)
}

// Duration implements Mixer.
func (m FFmpegMixer) Duration(path string) (float64, error) { return m.Runner.ProbeDurationSec(path) }

// Job states.
const (
	StateIdle      = "idle"
	StateRunning   = "running"
	StateDone      = "done"
	StateError     = "error"
	StateCancelled = "cancelled"
)

// Status is the visible state of one job. It never contains credentials.
type Status struct {
	State     string         `json:"state"`
	Message   string         `json:"message,omitempty"`
	UpdatedAt *time.Time     `json:"updatedAt,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
}

// Capability says whether an integration can run and why not.
type Capability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Code      string `json:"code,omitempty"`
	Target    string `json:"target,omitempty"` // vault URL (no credentials)
}

// View is the per-take state served to the host UI.
type View struct {
	TakeID         string `json:"takeId"`
	PackageReady   bool   `json:"packageReady"`
	HasTranscript  bool   `json:"hasTranscript"`
	HasHandoff     bool   `json:"hasHandoff"`
	AutoTranscribe bool   `json:"autoTranscribe"`
	Capabilities   struct {
		// Transcript is the selected engine's capability [SL-P4-3].
		Transcript Capability `json:"transcript"`
		Minutes    Capability `json:"minutes"`
		Parachute  Capability `json:"parachute"`
		ProgramMix Capability `json:"programMix"`
	} `json:"capabilities"`
	Transcription TranscriptionView `json:"transcription"`
	// CanResume is true when a failed or cancelled transcript kept finished
	// tracks, so a retry only redoes the rest.
	CanResume  bool   `json:"canResume"`
	Transcript Status `json:"transcript"`
	Parachute  Status `json:"parachute"`
	ClipEditor Status `json:"clipEditor"`
}

// Errors returned by Start*.
var (
	ErrBusy        = errors.New("this action is already running for the take")
	ErrUnavailable = errors.New("integration unavailable")
	ErrNotRunning  = errors.New("no transcript is running for this take")
)

// Service owns the jobs.
type Service struct {
	Store   *store.Store
	Package Package
	// Builtin is the on-device recognizer broker (the Mac app), nil when the
	// build has none [SL-P4-3].
	Builtin BuiltinTranscriber
	// Minutes is nil when no minutes-cli was found; MinutesReason says why.
	Minutes       Transcriber
	MinutesReason string
	// Language is the transcription language (default en-US).
	Language string
	// Parachute is nil when no vault is configured; ParachuteReason says why.
	Parachute       NoteSender
	ParachuteReason string
	ParachuteFolder string
	Mixer           Mixer
	// StateDir holds integration state (0700), e.g. vault note receipts and
	// the transcription engine choice.
	StateDir string

	mu       sync.Mutex
	jobs     map[string]*takeJobs
	wg       sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
	support  error // cached minutes Supported() result
	checked  bool
	checking sync.Mutex
	engine   string // selected engine, loaded lazily
}

type takeJobs struct {
	transcript, parachute, clip Status
	cancelTranscript            context.CancelFunc
	// kept holds finished tracks of a failed or cancelled run, keyed by
	// engine|manifest|source, so a retry redoes only the rest.
	kept map[string]*trackResult
}

func (s *Service) init() {
	if s.jobs == nil {
		s.jobs = map[string]*takeJobs{}
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
}

// Close cancels running jobs and waits for them.
func (s *Service) Close() {
	s.mu.Lock()
	s.init()
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

// Wait blocks until running jobs finish (tests).
func (s *Service) Wait() { s.wg.Wait() }

func now() *time.Time { t := time.Now().UTC(); return &t }

func (s *Service) job(takeID string) *takeJobs {
	s.init()
	j := s.jobs[takeID]
	if j == nil {
		idle := Status{State: StateIdle}
		j = &takeJobs{transcript: idle, parachute: idle, clip: idle}
		s.jobs[takeID] = j
	}
	return j
}

func (s *Service) set(takeID string, pick func(*takeJobs) *Status, st Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.UpdatedAt = now()
	*pick(s.job(takeID)) = st
}

// minutesSupport checks once that the CLI offers transcribe-file.
func (s *Service) minutesSupport() error {
	if s.Minutes == nil {
		return ErrUnavailable
	}
	s.checking.Lock()
	defer s.checking.Unlock()
	if s.checked && s.support == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s.support, s.checked = s.Minutes.Supported(ctx), true
	return s.support
}

// View reports capabilities and job states for a take.
func (s *Service) View(takeID string) View {
	v := View{TakeID: takeID}
	if dir, err := s.Package.PublishedPackageDir(takeID); err == nil {
		v.PackageReady = true
		v.HasTranscript = fileExists(filepath.Join(dir, transcript.Dir, "transcript.json"))
		v.HasHandoff = fileExists(filepath.Join(dir, handoff.Dir, "clip-editor.json"))
	}
	v.Transcription = s.transcriptionView()
	for _, e := range v.Transcription.Engines {
		if e.ID == EngineMinutes {
			v.Capabilities.Minutes = Capability{Available: e.Available, Reason: e.Reason, Code: e.Code}
		}
		if e.ID == v.Transcription.Engine {
			v.Capabilities.Transcript = Capability{Available: e.Available, Reason: e.Reason, Code: e.Code, Target: e.Detail}
		}
	}
	if s.Parachute != nil {
		v.Capabilities.Parachute = Capability{Available: true, Target: s.Parachute.VaultURL()}
	} else {
		v.Capabilities.Parachute.Reason = s.ParachuteReason
		if v.Capabilities.Parachute.Reason == "" {
			v.Capabilities.Parachute.Reason = "no Parachute vault configured (--parachute-url)"
		}
	}
	v.Capabilities.ProgramMix.Available = s.Mixer != nil
	if s.Mixer == nil {
		v.Capabilities.ProgramMix.Reason = "ffmpeg unavailable"
	}
	v.AutoTranscribe = s.autoTranscribe(takeID)
	s.mu.Lock()
	j := s.job(takeID)
	v.Transcript, v.Parachute, v.ClipEditor = j.transcript, j.parachute, j.clip
	v.CanResume = len(j.kept) > 0 && j.transcript.State != StateRunning
	s.mu.Unlock()
	return v
}

func fileExists(p string) bool {
	info, err := os.Lstat(p)
	return err == nil && info.Mode().IsRegular()
}

func (s *Service) autoTranscribe(takeID string) bool {
	if s.Store == nil {
		return false
	}
	tk, err := s.Store.GetTake(takeID)
	if err != nil {
		return false
	}
	sess, err := s.Store.GetSession(tk.SessionID)
	if err != nil || sess.PresetJSON == "" {
		return false
	}
	settings := store.DefaultSessionSettings()
	if json.Unmarshal([]byte(sess.PresetJSON), &settings) != nil {
		return false
	}
	return settings.AutoTranscribe
}

// TakeExported is called after a successful export: it starts the transcript
// when the session asked for auto-transcription. When the selected engine
// can't run, the transcript shows that as an error instead of failing
// silently.
func (s *Service) TakeExported(takeID string) {
	if !s.autoTranscribe(takeID) {
		return
	}
	err := s.StartTranscript(takeID)
	if err == nil || errors.Is(err, ErrBusy) {
		return
	}
	slog.Warn("Auto-transcribe not started", "takeId", takeID, "err", err)
	s.set(takeID, pickTranscript, Status{State: StateError, Message: "Auto-transcribe could not start: " + strings.TrimPrefix(err.Error(), ErrUnavailable.Error()+": ")})
}

func (s *Service) start(takeID string, pick func(*takeJobs) *Status, run func(ctx context.Context) (string, map[string]any, error)) error {
	s.mu.Lock()
	j := s.job(takeID)
	st := pick(j)
	if st.State == StateRunning {
		s.mu.Unlock()
		return ErrBusy
	}
	*st = Status{State: StateRunning, Message: "Starting…", UpdatedAt: now()}
	ctx, cancel := context.WithCancel(s.ctx)
	if st == &j.transcript {
		j.cancelTranscript = cancel
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer cancel()
		msg, detail, err := run(ctx)
		switch {
		case err != nil && (errors.Is(err, context.Canceled) || ctx.Err() != nil):
			s.set(takeID, pick, Status{State: StateCancelled, Message: "Cancelled. Nothing was written to the package.", Detail: detail})
		case err != nil:
			s.set(takeID, pick, Status{State: StateError, Message: err.Error(), Detail: detail})
		default:
			s.set(takeID, pick, Status{State: StateDone, Message: msg, Detail: detail})
		}
	}()
	return nil
}

func (s *Service) progress(takeID string, pick func(*takeJobs) *Status, msg string) {
	s.set(takeID, pick, Status{State: StateRunning, Message: msg})
}

func pickTranscript(j *takeJobs) *Status { return &j.transcript }
func pickParachute(j *takeJobs) *Status  { return &j.parachute }
func pickClip(j *takeJobs) *Status       { return &j.clip }

// StartTranscript transcribes every participant's isolated track with the
// selected engine, merges them on the take timeline and writes transcript/
// and the refreshed clip-editor handoff into the package.
func (s *Service) StartTranscript(takeID string) error {
	engine := s.TranscriptionEngine()
	if info := s.engineInfo(engine); !info.Available {
		return fmt.Errorf("%w: %s", ErrUnavailable, info.Reason)
	}
	if _, err := s.Package.PublishedPackageDir(takeID); err != nil {
		return err
	}
	return s.start(takeID, pickTranscript, func(ctx context.Context) (string, map[string]any, error) {
		return s.runTranscript(ctx, takeID, engine)
	})
}

// CancelTranscript stops a running transcript. Finished tracks are kept so
// a retry only redoes the rest; the package is not touched.
func (s *Service) CancelTranscript(takeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.job(takeID)
	if j.transcript.State != StateRunning || j.cancelTranscript == nil {
		return ErrNotRunning
	}
	j.cancelTranscript()
	j.transcript.Message = "Cancelling…"
	return nil
}

// transcriptProgress publishes the per-track table while a job runs.
func (s *Service) transcriptProgress(takeID, engine, msg string, tracks []TrackProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.job(takeID)
	if j.transcript.State != StateRunning {
		return // finished or cancelled meanwhile
	}
	j.transcript = Status{State: StateRunning, Message: msg, UpdatedAt: now(), Detail: map[string]any{"engine": engine, "tracks": tracks}}
}

func keepKey(engine string, rawManifest []byte, sourceID string) string {
	sum := sha256.Sum256(rawManifest)
	return engine + "|" + hex.EncodeToString(sum[:8]) + "|" + sourceID
}

func (s *Service) keptTrack(takeID, key string) *trackResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.job(takeID).kept[key]
}

func (s *Service) keepTrack(takeID, key string, r *trackResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.job(takeID)
	if j.kept == nil {
		j.kept = map[string]*trackResult{}
	}
	j.kept[key] = r
}

func (s *Service) dropKept(takeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.job(takeID).kept = nil
}

func (s *Service) runTranscript(ctx context.Context, takeID, engineID string) (string, map[string]any, error) {
	dir, err := s.Package.PublishedPackageDir(takeID)
	if err != nil {
		return "", nil, err
	}
	man, rawManifest, err := export.ReadManifest(dir)
	if err != nil {
		return "", nil, err
	}
	voices := man.SpeakerAudio()
	if len(voices) == 0 {
		return "", nil, errors.New("the package has no isolated voice tracks to transcribe")
	}
	progress := make([]TrackProgress, len(voices))
	for i, v := range voices {
		progress[i] = TrackProgress{SourceID: v.ID, Speaker: v.Participant, State: TrackQueued, DurationSec: v.DurationSec}
	}
	// pm guards progress: the engine's progress callback runs on the
	// provider's reader goroutine and may race a cancellation.
	var pm sync.Mutex
	snap := func() []TrackProgress {
		pm.Lock()
		defer pm.Unlock()
		return append([]TrackProgress(nil), progress...)
	}
	update := func(fn func()) { pm.Lock(); fn(); pm.Unlock() }
	cancelRest := func(from int) {
		update(func() {
			for k := from; k < len(progress); k++ {
				progress[k].State = TrackCancelled
			}
		})
	}
	detail := func() map[string]any { return map[string]any{"engine": engineID, "tracks": snap()} }
	var tracks []transcript.Track
	var speakers []transcript.Speaker
	var engine transcript.Engine
	var firstErr error
	for i, v := range voices {
		if ctx.Err() != nil {
			cancelRest(i)
			return "", detail(), ctx.Err()
		}
		sp := transcript.Speaker{Name: v.Participant, ParticipantID: v.ParticipantID, SourceID: v.ID, File: v.FileRelPath, OffsetSec: v.OffsetSec}
		key := keepKey(engineID, rawManifest, v.ID)
		res := s.keptTrack(takeID, key)
		if res != nil {
			update(func() { progress[i].Reused = true })
		} else {
			label := fmt.Sprintf("Transcribing %s (%d of %d)…", v.Participant, i+1, len(voices))
			update(func() { progress[i].State = TrackRunning })
			s.transcriptProgress(takeID, engineID, label, snap())
			idx, dur := i, v.DurationSec
			res, err = s.transcribeTrack(ctx, engineID, trackRequest{
				path: filepath.Join(dir, filepath.FromSlash(v.FileRelPath)), speaker: v.Participant, durationSec: dur,
				progress: func(sec float64) {
					if dur <= 0 || ctx.Err() != nil {
						return
					}
					running := false
					update(func() {
						if running = progress[idx].State == TrackRunning; running {
							progress[idx].Progress = math.Min(0.99, math.Round(sec/dur*1000)/1000)
						}
					})
					if running {
						s.transcriptProgress(takeID, engineID, label, snap())
					}
				},
			})
			if err != nil {
				if ctx.Err() != nil {
					cancelRest(i)
					return "", detail(), ctx.Err()
				}
				msg := err.Error()
				sp.Status, sp.Error = "error", msg
				update(func() { progress[i].State, progress[i].Error = TrackError, msg })
				if firstErr == nil {
					firstErr = fmt.Errorf("%s: %w", v.Participant, err)
				}
				speakers = append(speakers, sp)
				slog.Warn("Transcript track failed", "takeId", takeID, "sourceId", v.ID, "engine", engineID, "err", err)
				continue
			}
			s.keepTrack(takeID, key, res)
		}
		engine = res.engine
		tr := transcript.Track{Speaker: v.Participant, ParticipantID: v.ParticipantID, SourceID: v.ID, File: v.FileRelPath, OffsetSec: v.OffsetSec, DurationSec: v.DurationSec, Segments: res.segments}
		sp.Segments = len(tr.Segments)
		sp.Status = "ok"
		update(func() {
			progress[i].State, progress[i].Progress, progress[i].Segments = TrackDone, 1, sp.Segments
			if sp.Segments == 0 {
				progress[i].State = TrackEmpty
			}
		})
		if sp.Segments == 0 {
			sp.Status = "empty"
		}
		speakers = append(speakers, sp)
		tracks = append(tracks, tr)
	}
	if len(tracks) == 0 {
		d := detail()
		d["speakers"] = speakers
		return "", d, firstErr
	}
	if ctx.Err() != nil {
		return "", detail(), ctx.Err()
	}
	var end float64
	for _, src := range man.Sources {
		end = math.Max(end, src.OffsetSec+src.DurationSec)
	}
	doc := transcript.Document{
		Schema: transcript.Schema, Session: transcript.SessionMeta{ID: man.Session.ID, Title: man.Session.Title}, Take: man.Take,
		StartWallClock: man.StartWallClock, DurationSec: math.Round(end*1000) / 1000, Timeline: transcript.TimelineNote,
		Engine: engine, Speakers: speakers, Segments: transcript.Merge(tracks),
	}
	s.transcriptProgress(takeID, engineID, "Writing transcript into the export package…", snap())
	err = s.Package.WithPublishedPackage(takeID, func(current string) error {
		if current != dir {
			return errors.New("the export package was regenerated during transcription; run Transcribe again")
		}
		_, again, err := export.ReadManifest(current)
		if err != nil || !bytes.Equal(again, rawManifest) {
			return errors.New("the export package changed during transcription; run Transcribe again")
		}
		if err := transcript.Write(current, doc); err != nil {
			return err
		}
		_, err = s.writeHandoff(current, man, &doc)
		return err
	})
	d := detail()
	d["speakers"], d["segments"] = speakers, len(doc.Segments)
	if err != nil {
		return "", d, err
	}
	if firstErr == nil {
		s.dropKept(takeID)
	}
	s.set(takeID, pickClip, Status{State: StateDone, Message: "Clip-editor handoff refreshed with the transcript."})
	msg := fmt.Sprintf("Transcript ready: %d segments from %d speakers.", len(doc.Segments), len(tracks))
	if firstErr != nil {
		msg += " Some tracks failed: " + firstErr.Error() + " Retry to transcribe only the failed tracks."
	}
	return msg, d, nil
}

// writeHandoff renders the program mix when possible and writes the
// clip-editor handoff. The caller holds the package lock.
func (s *Service) writeHandoff(dir string, man *export.Manifest, doc *transcript.Document) (handoff.Document, error) {
	var programDur float64
	if s.Mixer != nil {
		var inputs []ffmpeg.MixInput
		for _, v := range man.SpeakerAudio() {
			inputs = append(inputs, ffmpeg.MixInput{Path: filepath.Join(dir, filepath.FromSlash(v.FileRelPath)), OffsetSec: v.OffsetSec})
		}
		if len(inputs) > 0 {
			if err := os.MkdirAll(filepath.Join(dir, handoff.Dir), 0o755); err != nil {
				return handoff.Document{}, err
			}
			out := filepath.Join(dir, filepath.FromSlash(handoff.ProgramFile))
			tmp := filepath.Join(filepath.Dir(out), ".program-mix.wav")
			if err := s.Mixer.Mix(inputs, tmp); err != nil {
				_ = os.Remove(tmp)
				slog.Warn("Program mix failed; handoff written without it", "err", err)
			} else if err := os.Rename(tmp, out); err != nil {
				return handoff.Document{}, err
			} else if d, err := s.Mixer.Duration(out); err == nil {
				programDur = d
			}
		}
	}
	var files []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, e.Name())
		}
	}
	return handoff.Write(dir, handoff.Input{Manifest: man, Transcript: doc, ProgramDurationSec: programDur, Files: files})
}

// WriteClipEditor writes (or refreshes) the clip-editor handoff now,
// including the transcript when one exists.
func (s *Service) WriteClipEditor(takeID string) error {
	var result handoff.Document
	err := s.Package.WithPublishedPackage(takeID, func(dir string) error {
		man, _, err := export.ReadManifest(dir)
		if err != nil {
			return err
		}
		doc, err := transcript.Read(dir)
		if err != nil {
			doc = nil
		}
		result, err = s.writeHandoff(dir, man, doc)
		return err
	})
	if err != nil {
		s.set(takeID, pickClip, Status{State: StateError, Message: err.Error()})
		return err
	}
	msg := "Clip-editor handoff written: handoff/clip-editor.json."
	if result.Transcript == nil {
		msg += " Transcribe the take to add the transcript."
	}
	s.set(takeID, pickClip, Status{State: StateDone, Message: msg, Detail: map[string]any{"program": result.Program != nil, "transcript": result.Transcript != nil}})
	return nil
}

// StartParachute sends (or updates) the take's session note in the vault.
func (s *Service) StartParachute(takeID string) error {
	if s.Parachute == nil {
		reason := s.ParachuteReason
		if reason == "" {
			reason = "no Parachute vault configured"
		}
		return fmt.Errorf("%w: %s", ErrUnavailable, reason)
	}
	if _, err := s.Package.PublishedPackageDir(takeID); err != nil {
		return err
	}
	return s.start(takeID, pickParachute, func(ctx context.Context) (string, map[string]any, error) {
		return s.runParachute(ctx, takeID)
	})
}

func (s *Service) runParachute(ctx context.Context, takeID string) (string, map[string]any, error) {
	dir, err := s.Package.PublishedPackageDir(takeID)
	if err != nil {
		return "", nil, err
	}
	man, _, err := export.ReadManifest(dir)
	if err != nil {
		return "", nil, err
	}
	doc, err := transcript.Read(dir)
	if err != nil {
		doc = nil
	}
	session := SessionFromManifest(man, takeID, dir)
	note := parachute.SessionNote(s.folder(), session, doc)
	receipts, err := s.loadReceipts()
	if err != nil {
		return "", nil, err
	}
	key := s.Parachute.VaultURL() + "|" + takeID
	var previous *parachute.Receipt
	if r, ok := receipts[key]; ok {
		previous = &r
	}
	s.progress(takeID, pickParachute, "Saving the session note to the vault…")
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	receipt, err := s.Parachute.SaveNote(ctx, note, previous)
	if err != nil {
		slog.Warn("Parachute session note failed", "takeId", takeID, "err", err)
		return "", nil, err
	}
	receipts[key] = receipt
	if err := s.saveReceipts(receipts); err != nil {
		return "", nil, fmt.Errorf("note saved, but its receipt could not be stored: %w", err)
	}
	verb := "Saved"
	if previous != nil && previous.ID == receipt.ID {
		verb = "Updated"
	}
	detail := map[string]any{"noteId": receipt.ID, "path": receipt.Path, "vault": s.Parachute.VaultURL(), "withTranscript": doc != nil}
	return fmt.Sprintf("%s in the vault: %s", verb, receipt.Path), detail, nil
}

func (s *Service) folder() string {
	if strings.TrimSpace(s.ParachuteFolder) == "" {
		return parachute.DefaultFolder
	}
	return s.ParachuteFolder
}

// SessionFromManifest describes a take for its vault note.
func SessionFromManifest(man *export.Manifest, takeID, dir string) parachute.Session {
	started, _ := time.Parse(time.RFC3339Nano, man.StartWallClock)
	var end float64
	seen := map[string]bool{}
	var names []string
	for _, src := range man.Sources {
		end = math.Max(end, src.OffsetSec+src.DurationSec)
		if src.Participant != "" && !seen[src.Participant] {
			seen[src.Participant] = true
			names = append(names, src.Participant)
		}
	}
	var files []string
	for _, f := range []string{"manifest.json", fmt.Sprintf("take_%02d_multicam.fcpxml", man.Take), fmt.Sprintf("take_%02d_premiere.xml", man.Take),
		"transcript/transcript.md", "transcript/transcript.srt", "transcript/transcript.vtt", "handoff/clip-editor.json"} {
		if fileExists(filepath.Join(dir, filepath.FromSlash(f))) {
			files = append(files, f)
		}
	}
	return parachute.Session{SessionID: man.Session.ID, TakeID: takeID, Title: man.Session.Title, Take: man.Take, Started: started,
		DurationSec: end, Participants: names, PackageDir: dir, Files: files}
}

func (s *Service) receiptsPath() string { return filepath.Join(s.StateDir, "parachute-receipts.json") }

func (s *Service) loadReceipts() (map[string]parachute.Receipt, error) {
	out := map[string]parachute.Receipt{}
	if s.StateDir == "" {
		return out, nil
	}
	data, err := os.ReadFile(s.receiptsPath())
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read vault receipts: %w", err)
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode vault receipts: %w", err)
	}
	return out, nil
}

func (s *Service) saveReceipts(r map[string]parachute.Receipt) error {
	if s.StateDir == "" {
		return nil
	}
	if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.receiptsPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.receiptsPath())
}
