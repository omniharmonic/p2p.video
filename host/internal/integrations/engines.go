package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/filespeech"
	"github.com/omniharmonic/studiolink/host/internal/minutes"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

// Transcription engines [SL-P4-3]. "builtin" is the on-device recognizer in
// the p2p.video Mac app (filespeech); "minutes" is an optional minutes-cli
// with transcribe-file. Both produce the same transcript.Document.
const (
	EngineBuiltin = "builtin"
	EngineMinutes = "minutes"
)

// Engine names written to transcript.json's engine.name.
const (
	EngineNameBuiltin = "studiolink-builtin"
	EngineNameMinutes = "minutes-cli"
	// BuiltinEngineVersion identifies the provider protocol that produced a
	// built-in transcript.
	BuiltinEngineVersion = "studiolink.file-transcription/1"
)

// BuiltinTranscriber is filespeech.Broker as the service uses it.
type BuiltinTranscriber interface {
	Status() filespeech.Status
	Transcribe(ctx context.Context, req filespeech.Request) (*filespeech.Result, error)
}

// EngineInfo describes one engine for the engine picker.
type EngineInfo struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
	// Code is a stable reason code when unavailable: no_provider,
	// not_authorized, unavailable, not_found, unsupported.
	Code   string `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Detail names the recognizer when available (no secrets).
	Detail string `json:"detail,omitempty"`
}

// TranscriptionView is the engine choice and what each engine can do now.
type TranscriptionView struct {
	Engine   string       `json:"engine"`
	Engines  []EngineInfo `json:"engines"`
	Language string       `json:"language"`
}

// TrackProgress is the per-track state of a transcript job.
type TrackProgress struct {
	SourceID    string  `json:"sourceId"`
	Speaker     string  `json:"speaker"`
	State       string  `json:"state"` // queued | running | done | empty | error | cancelled
	Progress    float64 `json:"progress"`
	DurationSec float64 `json:"durationSec"`
	Segments    int     `json:"segments"`
	Reused      bool    `json:"reused,omitempty"`
	Error       string  `json:"error,omitempty"`
}

// Track states.
const (
	TrackQueued    = "queued"
	TrackRunning   = "running"
	TrackDone      = "done"
	TrackEmpty     = "empty"
	TrackError     = "error"
	TrackCancelled = "cancelled"
)

// ErrUnknownEngine rejects an engine id that does not exist.
var ErrUnknownEngine = errors.New("unknown transcription engine")

const minutesMissing = "Minutes is optional. Install a minutes-cli with transcribe-file (or pass --minutes-cli) to use it."

// engines lists both engines with their current availability.
func (s *Service) engines() []EngineInfo {
	return []EngineInfo{s.builtinInfo(), s.minutesInfo()}
}

func (s *Service) builtinInfo() EngineInfo {
	info := EngineInfo{ID: EngineBuiltin, Label: "Built-in (this Mac)"}
	if s.Builtin == nil {
		info.Code, info.Reason = "unavailable", "This host build has no built-in transcription."
		return info
	}
	st := s.Builtin.Status()
	ok, code, reason := st.Available()
	info.Available, info.Code, info.Reason = ok, code, reason
	if ok {
		info.Detail = strings.TrimSpace(st.Name + " (" + st.Engine + ")")
	}
	return info
}

func (s *Service) minutesInfo() EngineInfo {
	info := EngineInfo{ID: EngineMinutes, Label: "Minutes"}
	if err := s.minutesSupport(); err != nil {
		info.Code, info.Reason = "not_found", minutesMissing
		switch {
		case s.Minutes != nil:
			info.Code, info.Reason = "unsupported", err.Error()
		case s.MinutesReason != "" && !strings.Contains(s.MinutesReason, minutes.ErrNotFound.Error()):
			// e.g. a configured --minutes-cli path that does not exist.
			info.Reason = s.MinutesReason
		}
		return info
	}
	info.Available = true
	return info
}

func (s *Service) engineInfo(id string) EngineInfo {
	if id == EngineMinutes {
		return s.minutesInfo()
	}
	return s.builtinInfo()
}

func (s *Service) prefPath() string { return filepath.Join(s.StateDir, "transcription.json") }

type enginePref struct {
	Engine string `json:"engine"`
}

// TranscriptionEngine is the selected engine (default built-in).
func (s *Service) TranscriptionEngine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine != "" {
		return s.engine
	}
	s.engine = EngineBuiltin
	if s.StateDir != "" {
		if data, err := os.ReadFile(s.prefPath()); err == nil {
			var p enginePref
			if json.Unmarshal(data, &p) == nil && (p.Engine == EngineBuiltin || p.Engine == EngineMinutes) {
				s.engine = p.Engine
			}
		}
	}
	return s.engine
}

// SetTranscriptionEngine selects and persists the engine. Minutes can only
// be chosen when a compatible minutes-cli is detected; the built-in engine
// can always be chosen (its status explains what is missing).
func (s *Service) SetTranscriptionEngine(id string) error {
	switch id {
	case EngineBuiltin:
	case EngineMinutes:
		if info := s.minutesInfo(); !info.Available {
			return fmt.Errorf("%w: %s", ErrUnavailable, info.Reason)
		}
	default:
		return ErrUnknownEngine
	}
	if s.StateDir != "" {
		if err := os.MkdirAll(s.StateDir, 0o700); err != nil {
			return fmt.Errorf("save transcription engine: %w", err)
		}
		data, _ := json.Marshal(enginePref{Engine: id})
		tmp := s.prefPath() + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return fmt.Errorf("save transcription engine: %w", err)
		}
		if err := os.Rename(tmp, s.prefPath()); err != nil {
			return fmt.Errorf("save transcription engine: %w", err)
		}
	}
	s.mu.Lock()
	s.engine = id
	s.mu.Unlock()
	return nil
}

func (s *Service) language() string {
	if s.Language != "" {
		return s.Language
	}
	return "en-US"
}

func (s *Service) transcriptionView() TranscriptionView {
	return TranscriptionView{Engine: s.TranscriptionEngine(), Engines: s.engines(), Language: s.language()}
}

// trackRequest is one isolated voice to transcribe.
type trackRequest struct {
	path, speaker string
	durationSec   float64
	progress      func(processedSec float64)
}

// trackResult is one transcribed voice in the engine-neutral shape.
type trackResult struct {
	engine   transcript.Engine
	segments []transcript.Segment // file-relative times
}

// transcribeTrack runs one voice through the chosen engine.
func (s *Service) transcribeTrack(ctx context.Context, engine string, req trackRequest) (*trackResult, error) {
	if engine == EngineMinutes {
		if s.Minutes == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnavailable, minutesMissing)
		}
		ft, err := s.Minutes.TranscribeFile(ctx, req.path, req.speaker, req.durationSec)
		if err != nil {
			return nil, err
		}
		return fromMinutes(ft), nil
	}
	if s.Builtin == nil {
		return nil, fmt.Errorf("%w: this host build has no built-in transcription", ErrUnavailable)
	}
	res, err := s.Builtin.Transcribe(ctx, filespeech.Request{Path: req.path, Language: s.language(), DurationSec: req.durationSec, Progress: req.progress})
	if err != nil {
		return nil, err
	}
	return fromBuiltin(res, s.language()), nil
}

func fromMinutes(ft *minutes.FileTranscript) *trackResult {
	out := &trackResult{engine: transcript.Engine{Name: EngineNameMinutes, Version: ft.Schema, ASR: ft.Engine.ASR, Locale: ft.Engine.Locale}}
	for _, seg := range ft.Segments {
		ts := transcript.Segment{Start: seg.Start, End: seg.End, Text: seg.Text, Confidence: seg.Confidence}
		for _, w := range seg.Words {
			ts.Words = append(ts.Words, transcript.Word{Word: w.Word, Start: w.Start, End: w.End, Confidence: w.Confidence})
		}
		out.segments = append(out.segments, ts)
	}
	return out
}

func fromBuiltin(res *filespeech.Result, language string) *trackResult {
	asr := res.ASR
	if asr == "" {
		asr = res.Engine
	}
	locale := res.Locale
	if locale == "" {
		locale = language
	}
	out := &trackResult{engine: transcript.Engine{Name: EngineNameBuiltin, Version: BuiltinEngineVersion, ASR: asr, Locale: locale}}
	for _, seg := range res.Segments {
		ts := transcript.Segment{Start: seg.Start, End: seg.End, Text: seg.Text, Confidence: seg.Confidence}
		for _, w := range seg.Words {
			ts.Words = append(ts.Words, transcript.Word{Word: w.Word, Start: w.Start, End: w.End, Confidence: w.Confidence})
		}
		out.segments = append(out.segments, ts)
	}
	return out
}
