// Package handoff writes the clip-editor handoff into an export package
// (ROADMAP P4, SL-P4-1, verified against ave in SL-P4-4):
// handoff/clip-editor.json describing every deliverable on the take timeline,
// plus files in the native shapes of the agentic video editor (`ave`):
// work/transcript.json, work/transcript.md, work/sync.json and the
// episode.json camera/speaker mapping. See docs/integrations/CLIP_EDITOR.md.
package handoff

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/export"
	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

// Schema identifies handoff/clip-editor.json. Additive fields keep it.
const Schema = "studiolink.clip-editor-handoff/1"

// Dir is the handoff folder inside an export package.
const Dir = "handoff"

// ProgramFile is the package-relative path of the reference program mix.
const ProgramFile = "handoff/program.wav"

// AveRawProgram is where the program mix goes inside an ave episode; ave's
// transcript and sync reference it.
const AveRawProgram = "raw/program.wav"

// Package-relative paths of the files in ave's native shapes.
const (
	AveTranscriptFile = "handoff/ave/transcript.json" // -> <episode>/work/transcript.json
	AveMarkdownFile   = "handoff/ave/transcript.md"   // -> <episode>/work/transcript.md
	AveSyncFile       = "handoff/ave/sync.json"       // -> <episode>/work/sync.json
	AveEpisodeFile    = "handoff/ave/episode.json"    // keys merged into <episode>/episode.json
)

// AveSession is the one ave session every p2p.video camera belongs to. A
// take is one session even when someone joined late; without it ave's ingest
// would split cameras of different lengths into separate sessions.
const AveSession = "s1"

// AveImportCommand is the one-command import (ave branch
// feat/studiolink-import; scripts/ave-import.sh falls back to the manual
// recipe for an ave without it).
const AveImportCommand = "ave import-studiolink <this package folder or its editor ZIP> <episode folder>"

// Speaker is one participant with the isolated voice used for the transcript.
type Speaker struct {
	ID            string   `json:"id"` // S1, S2... (ave speaker ids; the host first)
	Name          string   `json:"name"`
	ParticipantID string   `json:"participantId,omitempty"`
	Role          string   `json:"role,omitempty"` // host or guest (from the manifest participants)
	Audio         string   `json:"audio"`
	Video         []string `json:"video"`
	OffsetSec     float64  `json:"offsetSec"`
}

// Media is a program or reference file.
type Media struct {
	File        string  `json:"file"`
	Kind        string  `json:"kind"`
	DurationSec float64 `json:"durationSec,omitempty"`
	Description string  `json:"description"`
}

// TranscriptFiles points at the package transcript.
type TranscriptFiles struct {
	JSON     string `json:"json"`
	Markdown string `json:"markdown"`
	SRT      string `json:"srt"`
	VTT      string `json:"vtt"`
	Segments int    `json:"segments"`
}

// Ave lists files in ave's native shapes and how to lay out an episode.
type Ave struct {
	Transcript         string `json:"transcript,omitempty"`         // AveTranscriptFile -> <episode>/work/transcript.json
	TranscriptMarkdown string `json:"transcriptMarkdown,omitempty"` // AveMarkdownFile -> <episode>/work/transcript.md
	Sync               string `json:"sync,omitempty"`               // AveSyncFile -> <episode>/work/sync.json
	Episode            string `json:"episode"`                      // AveEpisodeFile: cameras, speakers, audio_source for episode.json
	// Raw maps an ave episode path (raw/...) to the package file to copy there.
	Raw    map[string]string `json:"raw"`
	Import string            `json:"import"`
	Note   string            `json:"note"`
}

// Document is handoff/clip-editor.json.
type Document struct {
	Schema         string                       `json:"schema"`
	Generator      string                       `json:"generator"`
	Consumer       map[string]string            `json:"consumer"`
	Session        export.SessionMeta           `json:"session"`
	Take           int                          `json:"take"`
	StartWallClock string                       `json:"startWallClock"`
	FrameRate      string                       `json:"frameRate"`
	SampleRate     int                          `json:"sampleRate"`
	Timeline       map[string]any               `json:"timeline"`
	Program        *Media                       `json:"program"`
	Participants   []export.ManifestParticipant `json:"participants,omitempty"`
	Speakers       []Speaker                    `json:"speakers"`
	Sources        []export.ExportSource        `json:"sources"`
	Transcript     *TranscriptFiles             `json:"transcript"`
	Editor         map[string]string            `json:"editorTimelines"`
	Ave            Ave                          `json:"ave"`
	Alignment      map[string]string            `json:"alignment"`
}

// Input is everything the handoff describes.
type Input struct {
	Manifest   *export.Manifest
	Transcript *transcript.Document // nil until transcribed
	// ProgramDurationSec > 0 means handoff/program.wav exists.
	ProgramDurationSec float64
	// Files lists package-relative files present (to reference editor XML).
	Files []string
}

// Build assembles the handoff document.
func Build(in Input) Document {
	m := in.Manifest
	speakers := Speakers(m)
	var end float64
	for _, s := range m.Sources {
		end = math.Max(end, s.OffsetSec+s.DurationSec)
	}
	doc := Document{
		Schema: Schema, Generator: "studiolink",
		Consumer: map[string]string{"name": "agentic-video-editor", "cli": "ave", "docs": "docs/integrations/CLIP_EDITOR.md"},
		Session:  m.Session, Take: m.Take, StartWallClock: m.StartWallClock, FrameRate: m.FrameRate, SampleRate: m.SampleRate,
		Timeline:     map[string]any{"origin": "take zero: every offsetSec and transcript time is seconds after it", "durationSec": round3(end)},
		Participants: m.Participants,
		Speakers:     speakers, Sources: m.Sources, Editor: map[string]string{},
		Alignment: map[string]string{
			"method": alignmentMethod(m),
			"note":   "Offsets come from p2p.video's clock alignment (sidecar-coarse), not independently verified acoustic sync. Drift is already corrected in the exported files, so drift_ppm is 0.",
		},
	}
	if in.ProgramDurationSec > 0 {
		doc.Program = &Media{File: ProgramFile, Kind: "audio", DurationSec: round3(in.ProgramDurationSec),
			Description: "Reference mix of every isolated microphone on the take timeline (mono 48 kHz, unmastered). Use the isolated tracks for the final mix."}
	}
	if in.Transcript != nil {
		doc.Transcript = &TranscriptFiles{JSON: "transcript/transcript.json", Markdown: "transcript/transcript.md",
			SRT: "transcript/transcript.srt", VTT: "transcript/transcript.vtt", Segments: len(in.Transcript.Segments)}
	}
	for _, f := range in.Files {
		switch {
		case strings.HasSuffix(f, ".fcpxml"):
			doc.Editor["fcpxml"] = f
		case strings.HasSuffix(f, "_premiere.xml"):
			doc.Editor["xmeml"] = f
		}
	}
	doc.Ave = Ave{Raw: map[string]string{}, Episode: AveEpisodeFile, Import: AveImportCommand,
		Note: "One command: `ave import-studiolink` (or scripts/ave-import.sh in p2p.video). By hand: copy each package file to the episode path on the left, " +
			"copy the work files into <episode>/work/, and merge `episode` into <episode>/episode.json. See docs/integrations/CLIP_EDITOR.md."}
	for _, s := range m.Sources {
		doc.Ave.Raw["raw/"+s.FileRelPath] = s.FileRelPath
	}
	if doc.Program != nil {
		doc.Ave.Raw[AveRawProgram] = ProgramFile
		doc.Ave.Sync = AveSyncFile
		if in.Transcript != nil {
			doc.Ave.Transcript = AveTranscriptFile
			doc.Ave.TranscriptMarkdown = AveMarkdownFile
		}
	}
	return doc
}

func alignmentMethod(m *export.Manifest) string {
	methods := map[string]bool{}
	for _, s := range m.Sources {
		if s.Method != "" {
			methods[s.Method] = true
		}
	}
	var list []string
	for k := range methods {
		list = append(list, k)
	}
	if len(list) == 0 {
		return "unknown"
	}
	sort.Strings(list)
	return strings.Join(list, ",")
}

// Speakers lists one speaker per participant voice (export.SpeakerAudio),
// with that participant's camera files (screen shares excluded). The host
// comes first (S1), then everyone else in manifest order, so ids read the
// same way in every take of a session.
func Speakers(m *export.Manifest) []Speaker {
	roles := map[string]string{}
	for _, p := range m.Participants {
		roles[p.ID] = p.Role
	}
	audio := m.SpeakerAudio()
	sort.SliceStable(audio, func(i, j int) bool {
		return roles[audio[i].ParticipantID] == "host" && roles[audio[j].ParticipantID] != "host"
	})
	var out []Speaker
	for i, a := range audio {
		sp := Speaker{ID: fmt.Sprintf("S%d", i+1), Name: a.Participant, ParticipantID: a.ParticipantID, Role: roles[a.ParticipantID],
			Audio: a.FileRelPath, OffsetSec: a.OffsetSec, Video: []string{}}
		// The participant's own camera first, then a phone camera.
		for _, phone := range []bool{false, true} {
			for _, v := range m.Sources {
				if v.Kind == "video" && v.Source != export.SourceScreen && (v.Source == export.SourcePhone) == phone && sameParticipant(v, a) {
					sp.Video = append(sp.Video, v.FileRelPath)
				}
			}
		}
		out = append(out, sp)
	}
	return out
}

func sameParticipant(a, b export.ExportSource) bool {
	if a.ParticipantID != "" || b.ParticipantID != "" {
		return a.ParticipantID == b.ParticipantID
	}
	return a.Participant == b.Participant
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// Write stores handoff/clip-editor.json and handoff/ave/episode.json, and,
// when the program mix exists, handoff/ave/sync.json and (with a transcript)
// handoff/ave/transcript.{json,md}. Stale ave transcript files are removed.
func Write(pkgDir string, in Input) (Document, error) {
	doc := Build(in)
	dir := filepath.Join(pkgDir, Dir)
	if err := os.MkdirAll(filepath.Join(dir, "ave"), 0o755); err != nil {
		return doc, err
	}
	if err := writeJSON(filepath.Join(dir, "clip-editor.json"), doc); err != nil {
		return doc, err
	}
	if err := writeJSON(filepath.Join(pkgDir, filepath.FromSlash(AveEpisodeFile)), AveEpisode(in.Manifest, doc.Program != nil)); err != nil {
		return doc, err
	}
	if doc.Program != nil {
		if err := writeJSON(filepath.Join(pkgDir, filepath.FromSlash(AveSyncFile)), AveSync(in.Manifest, in.ProgramDurationSec)); err != nil {
			return doc, err
		}
	}
	if doc.Ave.Transcript != "" {
		t := AveTranscript(in.Manifest, in.Transcript, in.ProgramDurationSec)
		if err := writeJSON(filepath.Join(pkgDir, filepath.FromSlash(AveTranscriptFile)), t); err != nil {
			return doc, err
		}
		if err := writeFile(filepath.Join(pkgDir, filepath.FromSlash(AveMarkdownFile)), []byte(AveMarkdown(t))); err != nil {
			return doc, err
		}
	} else {
		_ = os.Remove(filepath.Join(pkgDir, filepath.FromSlash(AveTranscriptFile)))
		_ = os.Remove(filepath.Join(pkgDir, filepath.FromSlash(AveMarkdownFile)))
	}
	return doc, nil
}

func writeJSON(p string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(p, append(data, '\n'))
}

func writeFile(p string, data []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// AveCamera is one entry of ave's episode.json `cameras`.
type AveCamera struct {
	File        string `json:"file"`
	Role        string `json:"role"`    // host | guest (ave's planner only treats "wide" specially)
	Session     string `json:"session"` // always AveSession
	Participant string `json:"participant"`
}

// AveSpeaker is one entry of ave's episode.json `speakers`.
type AveSpeaker struct {
	Name          string `json:"name"`
	ParticipantID string `json:"participantId,omitempty"`
	Camera        string `json:"camera,omitempty"` // key of cameras; empty when the participant had no camera
}

// AveAngle is a camera file ave's planner should not cut to on its own (a
// second camera of the same person, or a phone camera). Screen shares are
// listed too, with source "screen".
type AveAngle struct {
	File        string `json:"file"`
	Participant string `json:"participant"`
	Source      string `json:"source,omitempty"`
}

// AveEpisodeMapping is handoff/ave/episode.json: the keys ave reads from
// episode.json at the camera-mapping checkpoint, exact by construction
// (every camera is its participant's own webcam).
type AveEpisodeMapping struct {
	Cameras     map[string]AveCamera  `json:"cameras"`
	Speakers    map[string]AveSpeaker `json:"speakers"`
	AudioSource string                `json:"audio_source,omitempty"`
	ExtraAngles []AveAngle            `json:"extra_angles,omitempty"`
}

// AveEpisode maps every speaker to their own camera: camera C<n> for speaker
// S<n> (their webcam, or their phone camera when they had no webcam). Further
// cameras and screen shares go to extra_angles, so ave's cutaways never land
// on a slide deck. audio_source is the program mix when it exists.
func AveEpisode(m *export.Manifest, hasProgram bool) AveEpisodeMapping {
	out := AveEpisodeMapping{Cameras: map[string]AveCamera{}, Speakers: map[string]AveSpeaker{}}
	mapped := map[string]bool{}
	for _, sp := range Speakers(m) {
		as := AveSpeaker{Name: sp.Name, ParticipantID: sp.ParticipantID}
		if len(sp.Video) > 0 {
			key := "C" + strings.TrimPrefix(sp.ID, "S")
			role := "guest"
			if sp.Role == "host" {
				role = "host"
			}
			out.Cameras[key] = AveCamera{File: path.Join("raw", sp.Video[0]), Role: role, Session: AveSession, Participant: sp.Name}
			as.Camera = key
			mapped[sp.Video[0]] = true
		}
		out.Speakers[sp.ID] = as
	}
	for _, s := range m.Sources {
		if s.Kind == "video" && !mapped[s.FileRelPath] {
			out.ExtraAngles = append(out.ExtraAngles, AveAngle{File: path.Join("raw", s.FileRelPath), Participant: s.Participant, Source: s.Source})
		}
	}
	if hasProgram {
		out.AudioSource = AveRawProgram
	}
	return out
}

// AveSync is ave's work/sync.json with the program mix as the reference
// (episode time = take time) and every package file placed at its manifest
// offset. Exported files are already drift-corrected, hence drift_ppm 0.
func AveSync(m *export.Manifest, programDur float64) map[string]any {
	files := map[string]any{
		AveRawProgram: map[string]any{"start_offset": 0.0, "drift_ppm": 0.0, "psr": nil, "psr_min": nil, "residual_ms": 0.0, "points": []any{}},
	}
	for _, s := range m.Sources {
		files[path.Join("raw", s.FileRelPath)] = map[string]any{
			"start_offset": round4(s.OffsetSec), "drift_ppm": 0.0, "psr": nil, "psr_min": nil,
			"residual_ms": s.ResidualMs, "points": []any{}, "method": "studiolink-manifest:" + s.Method,
		}
	}
	return map[string]any{
		"reference": AveRawProgram, "reference_duration": round3(programDur), "points_requested": 0,
		"files": files, "source": "p2p.video manifest.json (no audio correlation was run)",
		"warnings": []string{"Offsets come from p2p.video's clock alignment, not ave's audio correlation; spot-check sync on the assembly."},
	}
}

func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// aveUtteranceGap mirrors ave's UTTERANCE_GAP (src/ave/transcribe.py).
const aveUtteranceGap = 1.0

// AveWord is one word of ave's work/transcript.json.
type AveWord struct {
	W       string  `json:"w"`
	Start   float64 `json:"start"`
	End     float64 `json:"end"`
	Conf    float64 `json:"conf"`
	Speaker string  `json:"speaker"`
	Timing  string  `json:"timing,omitempty"` // "interpolated" when the recognizer gave no word times
}

// AveUtterance is one utterance of ave's work/transcript.json.
type AveUtterance struct {
	Speaker   string  `json:"speaker"`
	Start     float64 `json:"start"`
	End       float64 `json:"end"`
	FirstWord int     `json:"first_word"`
	LastWord  int     `json:"last_word"`
	Text      string  `json:"text"`
}

// AveSpeakerInfo is one entry of ave's transcript `speakers`.
type AveSpeakerInfo struct {
	TalkSeconds   float64 `json:"talk_seconds"`
	Name          string  `json:"name"`
	ParticipantID string  `json:"participantId,omitempty"`
	Track         string  `json:"track,omitempty"`
}

// AveTranscriptDoc is ave's work/transcript.json.
type AveTranscriptDoc struct {
	Source     string                     `json:"source"`
	Engine     map[string]string          `json:"engine"`
	Duration   float64                    `json:"duration"`
	Speakers   map[string]*AveSpeakerInfo `json:"speakers"`
	Words      []AveWord                  `json:"words"`
	Utterances []AveUtterance             `json:"utterances"`
}

// AveTranscript converts the merged transcript to ave's work/transcript.json
// shape on the program mix timeline: words {w,start,end,conf,speaker} sorted
// by time, utterances rebuilt with ave's rule (same speaker, gap ≤ 1 s), and
// speakers S1.. named from the participants. Segments without word timings
// get evenly spaced word times flagged with "timing": "interpolated".
func AveTranscript(m *export.Manifest, doc *transcript.Document, programDur float64) AveTranscriptDoc {
	ids := map[string]string{}
	speakers := map[string]*AveSpeakerInfo{}
	for _, sp := range Speakers(m) {
		key := sp.ParticipantID
		if key == "" {
			key = "name:" + sp.Name
		}
		ids[key] = sp.ID
		speakers[sp.ID] = &AveSpeakerInfo{Name: sp.Name, ParticipantID: sp.ParticipantID, Track: "raw/" + sp.Audio}
	}
	idFor := func(s transcript.Segment) string {
		key := s.ParticipantID
		if key == "" {
			key = "name:" + s.Speaker
		}
		if id, ok := ids[key]; ok {
			return id
		}
		id := fmt.Sprintf("S%d", len(ids)+1)
		ids[key] = id
		speakers[id] = &AveSpeakerInfo{Name: s.Speaker}
		return id
	}
	words := []AveWord{}
	for _, s := range doc.Segments {
		id := idFor(s)
		if len(s.Words) > 0 {
			for _, w := range s.Words {
				words = append(words, AveWord{W: w.Word, Start: w.Start, End: w.End, Conf: w.Confidence, Speaker: id})
			}
			continue
		}
		tokens := strings.Fields(s.Text)
		step := (s.End - s.Start) / float64(max(1, len(tokens)))
		for i, tok := range tokens {
			words = append(words, AveWord{W: tok, Start: round3(s.Start + step*float64(i)), End: round3(s.Start + step*float64(i+1)), Conf: 0, Speaker: id, Timing: "interpolated"})
		}
	}
	// Stable sort by start keeps each speaker's word order.
	sort.SliceStable(words, func(i, j int) bool { return words[i].Start < words[j].Start })
	utts := []AveUtterance{}
	for i, w := range words {
		if n := len(utts); n > 0 && utts[n-1].Speaker == w.Speaker && w.Start-utts[n-1].End <= aveUtteranceGap {
			utts[n-1].End = w.End
			utts[n-1].LastWord = i
			utts[n-1].Text += " " + w.W
			continue
		}
		utts = append(utts, AveUtterance{Speaker: w.Speaker, Start: w.Start, End: w.End, FirstWord: i, LastWord: i, Text: w.W})
	}
	for _, u := range utts {
		if sp := speakers[u.Speaker]; sp != nil {
			sp.TalkSeconds = round3(sp.TalkSeconds + (u.End - u.Start))
		}
	}
	return AveTranscriptDoc{
		Source:   AveRawProgram,
		Engine:   map[string]string{"asr": engineLabel(doc.Engine), "diarization": "studiolink-isolated-tracks"},
		Duration: round3(programDur),
		Speakers: speakers,
		Words:    words, Utterances: utts,
	}
}

// engineLabel names the recognizer that produced the p2p.video transcript.
func engineLabel(e transcript.Engine) string {
	label := e.Name
	if e.ASR != "" {
		if label != "" {
			label += ": "
		}
		label += e.ASR
	}
	if label == "" {
		return "unknown"
	}
	return label
}

// AveMarkdown is work/transcript.md exactly as `ave transcribe` writes it:
// one line per utterance, "[HH:MM:SS.ss] S1: text".
func AveMarkdown(t AveTranscriptDoc) string {
	var b strings.Builder
	for _, u := range t.Utterances {
		fmt.Fprintf(&b, "[%s] %s: %s\n", aveTimestamp(u.Start), u.Speaker, u.Text)
	}
	return b.String()
}

// aveTimestamp mirrors ave's transcribe.fmt_ts: HH:MM:SS.ss.
func aveTimestamp(t float64) string {
	h := math.Floor(t / 3600)
	rem := t - h*3600
	m := math.Floor(rem / 60)
	s := rem - m*60
	return fmt.Sprintf("%02d:%02d:%05.2f", int(h), int(m), s)
}
