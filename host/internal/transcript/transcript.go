// Package transcript merges per-speaker transcripts of isolated tracks into
// one take-aligned transcript and renders it as JSON, Markdown, SRT and WebVTT
// (ROADMAP P4, SL-P4-1). Each isolated microphone holds one participant, so a
// segment's speaker is the participant who owns the track: no diarization.
package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Schema identifies transcript/transcript.json. Additive changes keep it.
const Schema = "studiolink.transcript/1"

// Dir is the transcript folder inside an export package.
const Dir = "transcript"

// Word is one recognized word on the take timeline (seconds).
type Word struct {
	Word       string  `json:"word"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"`
}

// Segment is one utterance on the take timeline (seconds from take zero, the
// same origin as manifest.json offsets).
type Segment struct {
	Start         float64  `json:"start"`
	End           float64  `json:"end"`
	Speaker       string   `json:"speaker"`
	ParticipantID string   `json:"participantId,omitempty"`
	SourceID      string   `json:"sourceId"`
	Text          string   `json:"text"`
	Confidence    *float64 `json:"confidence,omitempty"`
	Words         []Word   `json:"words,omitempty"`
}

// Track is the transcript of one isolated file, with times relative to the
// start of that file, plus where the file sits on the take timeline.
type Track struct {
	Speaker       string
	ParticipantID string
	SourceID      string
	File          string  // package-relative path
	OffsetSec     float64 // manifest offsetSec
	DurationSec   float64
	Segments      []Segment // file-relative times; Speaker fields are ignored
}

// Speaker summarizes one transcribed track in the document.
type Speaker struct {
	Name          string  `json:"name"`
	ParticipantID string  `json:"participantId,omitempty"`
	SourceID      string  `json:"sourceId"`
	File          string  `json:"file"`
	OffsetSec     float64 `json:"offsetSec"`
	Segments      int     `json:"segments"`
	Status        string  `json:"status"` // "ok", "empty", "error"
	Error         string  `json:"error,omitempty"`
}

// Engine records what produced the transcript.
type Engine struct {
	Name    string `json:"name"`              // "minutes-cli"
	Version string `json:"version,omitempty"` // CLI schema, e.g. minutes.file-transcript/1
	ASR     string `json:"asr,omitempty"`
	Locale  string `json:"locale,omitempty"`
}

// SessionMeta mirrors the manifest's session object.
type SessionMeta struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Document is transcript/transcript.json.
type Document struct {
	Schema         string      `json:"schema"`
	Session        SessionMeta `json:"session"`
	Take           int         `json:"take"`
	StartWallClock string      `json:"startWallClock,omitempty"`
	DurationSec    float64     `json:"durationSec"`
	Timeline       string      `json:"timeline"`
	Engine         Engine      `json:"engine"`
	Speakers       []Speaker   `json:"speakers"`
	Segments       []Segment   `json:"segments"`
}

// TimelineNote explains the time origin to readers of the JSON.
const TimelineNote = "seconds from take zero; the origin of manifest.json offsetSec"

// Merge shifts every track's segments by its manifest offset, names them after
// the track's participant and returns one time-ordered list. Ties order by
// end time, then by track order, so output is deterministic. Words are shifted
// with their segment. Segments that would start before take zero are clamped.
func Merge(tracks []Track) []Segment {
	type keyed struct {
		seg   Segment
		track int
		index int
	}
	var all []keyed
	for ti, tr := range tracks {
		for si, s := range tr.Segments {
			text := strings.Join(strings.Fields(s.Text), " ")
			if text == "" {
				continue
			}
			shift := func(v float64) float64 { return round3(math.Max(0, v+tr.OffsetSec)) }
			out := Segment{
				Start: shift(s.Start), End: shift(s.End), Speaker: tr.Speaker,
				ParticipantID: tr.ParticipantID, SourceID: tr.SourceID, Text: text, Confidence: s.Confidence,
			}
			if out.End < out.Start {
				out.End = out.Start
			}
			for _, w := range s.Words {
				out.Words = append(out.Words, Word{Word: w.Word, Start: shift(w.Start), End: shift(w.End), Confidence: w.Confidence})
			}
			all = append(all, keyed{out, ti, si})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.seg.Start != b.seg.Start {
			return a.seg.Start < b.seg.Start
		}
		if a.seg.End != b.seg.End {
			return a.seg.End < b.seg.End
		}
		if a.track != b.track {
			return a.track < b.track
		}
		return a.index < b.index
	})
	segs := make([]Segment, len(all))
	for i, k := range all {
		segs[i] = k.seg
	}
	return segs
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// Timestamp renders seconds as HH:MM:SS<sep>mmm (SRT uses ',', WebVTT '.').
func Timestamp(sec float64, sep string) string {
	ms := int64(math.Round(math.Max(0, sec) * 1000))
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", ms/3_600_000, (ms/60_000)%60, (ms/1000)%60, sep, ms%1000)
}

// Timecode renders seconds the way Minutes labels transcript lines: M:SS, or
// H:MM:SS from one hour (MinutesCore SummaryCitations.timecode).
func Timecode(sec float64) string {
	total := int64(math.Max(0, sec))
	if total >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", total/3600, (total/60)%60, total%60)
	}
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// Cue is one caption.
type Cue struct {
	Start, End float64
	Speaker    string
	Text       string
}

// Caption limits: two 42-character lines, at most 7 s, at least 0.5 s on screen.
const (
	MaxCueChars = 84
	MaxCueSec   = 7.0
	MinCueSec   = 0.5
)

// Cues splits segments into caption-sized cues. With word timings, cue
// boundaries follow words; without them, time is shared out by characters.
func Cues(segs []Segment) []Cue {
	var cues []Cue
	for _, s := range segs {
		if len(s.Words) > 0 {
			cues = append(cues, wordCues(s)...)
		} else {
			cues = append(cues, textCues(s)...)
		}
	}
	for i := range cues {
		if cues[i].End-cues[i].Start < MinCueSec {
			cues[i].End = round3(cues[i].Start + MinCueSec)
		}
	}
	return cues
}

func wordCues(s Segment) []Cue {
	var cues []Cue
	var cur []Word
	flush := func() {
		if len(cur) == 0 {
			return
		}
		words := make([]string, len(cur))
		for i, w := range cur {
			words[i] = w.Word
		}
		cues = append(cues, Cue{Start: cur[0].Start, End: cur[len(cur)-1].End, Speaker: s.Speaker, Text: strings.Join(words, " ")})
		cur = nil
	}
	length := 0
	for _, w := range s.Words {
		if len(cur) > 0 && (length+1+len(w.Word) > MaxCueChars || w.End-cur[0].Start > MaxCueSec) {
			flush()
			length = 0
		}
		if len(cur) > 0 {
			length++
		}
		length += len(w.Word)
		cur = append(cur, w)
	}
	flush()
	return cues
}

func textCues(s Segment) []Cue {
	words := strings.Fields(s.Text)
	var chunks []string
	var cur []string
	length := 0
	for _, w := range words {
		if len(cur) > 0 && length+1+len(w) > MaxCueChars {
			chunks = append(chunks, strings.Join(cur, " "))
			cur, length = nil, 0
		}
		if len(cur) > 0 {
			length++
		}
		length += len(w)
		cur = append(cur, w)
	}
	if len(cur) > 0 {
		chunks = append(chunks, strings.Join(cur, " "))
	}
	// Respect the duration limit too: split further when each chunk would be too long on screen.
	dur := s.End - s.Start
	if n := int(math.Ceil(dur / MaxCueSec)); n > len(chunks) && len(words) >= n {
		chunks = splitEvenly(words, n)
	}
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	cues := make([]Cue, 0, len(chunks))
	at := s.Start
	for i, c := range chunks {
		end := s.End
		if i < len(chunks)-1 && total > 0 {
			end = round3(at + dur*float64(len(c))/float64(total))
		}
		cues = append(cues, Cue{Start: at, End: end, Speaker: s.Speaker, Text: c})
		at = end
	}
	return cues
}

func splitEvenly(words []string, n int) []string {
	out := make([]string, 0, n)
	per := float64(len(words)) / float64(n)
	start := 0
	for i := 1; i <= n; i++ {
		end := int(math.Round(per * float64(i)))
		if i == n {
			end = len(words)
		}
		if end > start {
			out = append(out, strings.Join(words[start:end], " "))
		}
		start = end
	}
	return out
}

// SRT renders SubRip captions, each prefixed with the speaker's name.
func SRT(segs []Segment) string {
	var b strings.Builder
	for i, c := range Cues(segs) {
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s: %s\n\n", i+1, Timestamp(c.Start, ","), Timestamp(c.End, ","), oneLine(c.Speaker), oneLine(c.Text))
	}
	return b.String()
}

// VTT renders WebVTT captions with voice spans (<v Name>).
func VTT(segs []Segment) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range Cues(segs) {
		fmt.Fprintf(&b, "%s --> %s\n<v %s>%s\n\n", Timestamp(c.Start, "."), Timestamp(c.End, "."), vttEscape(oneLine(c.Speaker)), vttEscape(oneLine(c.Text)))
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func vttEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "-->", "--&gt;").Replace(s)
}

// Markdown renders the transcript the way Minutes writes meeting notes: a
// title, then "**Name** (M:SS): text" lines under "## Transcript".
func Markdown(doc Document) string {
	var b strings.Builder
	title := strings.TrimSpace(doc.Session.Title)
	if title == "" {
		title = "p2p.video session"
	}
	fmt.Fprintf(&b, "# %s — Take %d\n\n", oneLine(title), doc.Take)
	b.WriteString(Body(doc))
	return b.String()
}

// Body is the transcript section alone (used inside vault notes).
func Body(doc Document) string {
	var b strings.Builder
	var names []string
	for _, s := range doc.Speakers {
		names = append(names, oneLine(s.Name))
	}
	if len(names) > 0 {
		fmt.Fprintf(&b, "Speakers: %s\n\n", strings.Join(names, ", "))
	}
	b.WriteString("## Transcript\n\n")
	if len(doc.Segments) == 0 {
		b.WriteString("_No speech was recognized._\n")
	}
	for _, s := range doc.Segments {
		fmt.Fprintf(&b, "**%s** (%s): %s\n\n", mdEscape(oneLine(s.Speaker)), Timecode(s.Start), oneLine(s.Text))
	}
	return b.String()
}

func mdEscape(s string) string { return strings.ReplaceAll(s, "*", `\*`) }

// Write stores transcript.json, transcript.md, transcript.srt and
// transcript.vtt in <pkgDir>/transcript, replacing a previous transcript only
// after every file is complete.
func Write(pkgDir string, doc Document) error {
	if doc.Schema == "" {
		doc.Schema = Schema
	}
	if doc.Timeline == "" {
		doc.Timeline = TimelineNote
	}
	if doc.Segments == nil {
		doc.Segments = []Segment{}
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode transcript: %w", err)
	}
	staging, err := os.MkdirTemp(pkgDir, ".transcript-")
	if err != nil {
		return fmt.Errorf("stage transcript: %w", err)
	}
	defer os.RemoveAll(staging)
	files := map[string][]byte{
		"transcript.json": append(data, '\n'),
		"transcript.md":   []byte(Markdown(doc)),
		"transcript.srt":  []byte(SRT(doc.Segments)),
		"transcript.vtt":  []byte(VTT(doc.Segments)),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("stage transcript: %w", err)
	}
	final := filepath.Join(pkgDir, Dir)
	old := final + ".old"
	_ = os.RemoveAll(old)
	if _, err := os.Lstat(final); err == nil {
		if err := os.Rename(final, old); err != nil {
			return fmt.Errorf("replace transcript: %w", err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		return errors.Join(fmt.Errorf("publish transcript: %w", err), os.Rename(old, final))
	}
	_ = os.RemoveAll(old)
	return nil
}

// Read loads transcript/transcript.json from a package.
func Read(pkgDir string) (*Document, error) {
	data, err := os.ReadFile(filepath.Join(pkgDir, Dir, "transcript.json"))
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode transcript: %w", err)
	}
	if doc.Schema != Schema {
		return nil, fmt.Errorf("unsupported transcript schema %q", doc.Schema)
	}
	return &doc, nil
}
