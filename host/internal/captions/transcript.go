package captions

import (
	"fmt"
	"sort"
	"strings"
)

// Segment is one final caption in a meeting transcript.
type Segment struct {
	ParticipantID string `json:"participantId"`
	Name          string `json:"name"`
	Text          string `json:"text"`
	StartMs       int64  `json:"startMs"` // host wall clock, Unix ms
	EndMs         int64  `json:"endMs"`
}

// Transcript is a meeting's final captions, ordered by start time. It is the
// live-captions complement to the post-take transcript built from recorded
// tracks; it is held in memory only and never written into a take or export.
type Transcript struct {
	SessionID string    `json:"sessionId"`
	Language  string    `json:"language"`
	Source    string    `json:"source"` // always "live-captions"
	Segments  []Segment `json:"segments"`
}

// Transcript returns a copy of a meeting's final captions.
func (m *Manager) Transcript(sessionID string) (Transcript, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[sessionID]
	if s == nil {
		return Transcript{}, false
	}
	t := Transcript{SessionID: sessionID, Language: s.language, Source: "live-captions", Segments: make([]Segment, 0, len(s.finals))}
	for _, c := range s.finals {
		t.Segments = append(t.Segments, Segment{ParticipantID: c.ParticipantID, Name: c.Name, Text: c.Text, StartMs: c.StartMs, EndMs: c.EndMs})
	}
	sort.SliceStable(t.Segments, func(i, j int) bool { return t.Segments[i].StartMs < t.Segments[j].StartMs })
	return t, true
}

// VTT renders the transcript as WebVTT with a voice span per speaker. Cue
// times are relative to the first segment.
func (t Transcript) VTT() string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	if len(t.Segments) == 0 {
		return b.String()
	}
	origin := t.Segments[0].StartMs
	for i, s := range t.Segments {
		end := s.EndMs
		if end <= s.StartMs {
			end = s.StartMs + 1000
		}
		fmt.Fprintf(&b, "%d\n%s --> %s\n<v %s>%s\n\n", i+1, vttTime(s.StartMs-origin), vttTime(end-origin), vttVoice(s.Name), vttEscape(s.Text))
	}
	return b.String()
}

func vttTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms%1000)
}

var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func vttEscape(s string) string { return vttEscaper.Replace(s) }

// vttVoice escapes a speaker name for a <v> annotation, which ends at '>'.
func vttVoice(name string) string {
	if name = strings.TrimSpace(name); name == "" {
		name = "Speaker"
	}
	return vttEscape(strings.ReplaceAll(name, "\n", " "))
}
