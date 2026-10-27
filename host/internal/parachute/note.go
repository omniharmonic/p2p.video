package parachute

import (
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/omniharmonic/studiolink/host/internal/transcript"
)

// DefaultFolder matches Minutes' default vault folder for meetings.
const DefaultFolder = "Meetings"

// Tags put on every p2p.video session note. "meeting" is the tag Minutes
// uses, so vault views over meetings include p2p.video sessions.
var Tags = []string{"meeting", "podcast", "studiolink"}

// Session describes one exported take for its vault note.
type Session struct {
	SessionID    string
	TakeID       string
	Title        string
	Take         int
	Started      time.Time
	DurationSec  float64
	Participants []string
	PackageDir   string   // absolute host path of the published export package
	Files        []string // package-relative deliverables worth listing
}

// NotePath follows Minutes' ParachuteMeetingNote.path: "<folder>/<yyyy-MM-dd HHmm> <title>",
// with path separators and control characters in the title replaced and the
// title bounded to 120 characters. Takes after the first are suffixed.
func NotePath(folder string, s Session) string {
	title := strings.TrimSpace(strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '\n', '\r', '\t':
			return '-'
		}
		return r
	}, s.Title))
	if title == "" {
		title = "p2p.video session"
	}
	if r := []rune(title); len(r) > 120 {
		title = string(r[:120])
	}
	if s.Take > 1 {
		title += fmt.Sprintf(" (Take %d)", s.Take)
	}
	var parts []string
	for _, p := range strings.Split(strings.Trim(folder, "/ "), "/") {
		if p != "" && p != "." && p != ".." {
			parts = append(parts, p)
		}
	}
	name := s.Started.Local().Format("2006-01-02 1504") + " " + title
	if len(parts) == 0 {
		return name
	}
	return strings.Join(parts, "/") + "/" + name
}

// Duration renders seconds as M:SS or H:MM:SS.
func Duration(sec float64) string {
	return transcript.Timecode(math.Round(sec))
}

// SessionNote builds the vault note: Markdown without front matter (the vault
// keeps metadata separately, as Minutes does), the session facts, then the
// transcript in Minutes' "**Name** (M:SS): text" form when one exists.
func SessionNote(folder string, s Session, doc *transcript.Document) Note {
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = "p2p.video session"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — Take %d\n\n", oneLine(title), s.Take)
	fmt.Fprintf(&b, "- **Date:** %s\n", s.Started.Local().Format("2006-01-02 15:04"))
	if len(s.Participants) > 0 {
		names := make([]string, len(s.Participants))
		for i, p := range s.Participants {
			names[i] = "[[" + oneLine(p) + "]]"
		}
		fmt.Fprintf(&b, "- **Participants:** %s\n", strings.Join(names, ", "))
	}
	if s.DurationSec > 0 {
		fmt.Fprintf(&b, "- **Duration:** %s\n", Duration(s.DurationSec))
	}
	b.WriteString("- **Recorded with:** p2p.video, one isolated track per participant (speaker names are exact)\n")
	if s.PackageDir != "" {
		link := (&url.URL{Scheme: "file", Path: filepath.ToSlash(s.PackageDir)}).String()
		fmt.Fprintf(&b, "- **Export package:** [%s](%s)\n", oneLine(filepath.Base(s.PackageDir)), link)
		fmt.Fprintf(&b, "  `%s`\n", oneLine(s.PackageDir))
	}
	if len(s.Files) > 0 {
		quoted := make([]string, len(s.Files))
		for i, f := range s.Files {
			quoted[i] = "`" + oneLine(f) + "`"
		}
		fmt.Fprintf(&b, "- **Files:** %s\n", strings.Join(quoted, ", "))
	}
	b.WriteString("\n")
	if doc != nil {
		b.WriteString(transcript.Body(*doc))
	} else {
		b.WriteString("## Transcript\n\n_No transcript yet. Use “Transcribe” in p2p.video’s export panel, then send again to update this note._\n")
	}
	meta := map[string]string{
		"source":                "studiolink",
		"studiolink_session_id": s.SessionID,
		"studiolink_take_id":    s.TakeID,
		"take":                  strconv.Itoa(s.Take),
		"date":                  s.Started.UTC().Format(time.RFC3339),
	}
	if len(s.Participants) > 0 {
		meta["attendees"] = strings.Join(s.Participants, ", ")
	}
	if s.DurationSec > 0 {
		meta["duration_seconds"] = strconv.FormatFloat(math.Round(s.DurationSec*10)/10, 'f', -1, 64)
	}
	if s.PackageDir != "" {
		meta["export_path"] = s.PackageDir
	}
	if doc != nil {
		meta["transcript"] = TranscriptSource(doc.Engine.Name)
	}
	return Note{Path: NotePath(folder, s), Content: strings.TrimRight(b.String(), "\n") + "\n", Tags: append([]string(nil), Tags...), Metadata: meta}
}

// TranscriptSource names the engine in note metadata: "minutes" for
// minutes-cli, "builtin" for p2p.video's on-device engine [SL-P4-3].
func TranscriptSource(engineName string) string {
	switch engineName {
	case "minutes-cli", "":
		return "minutes"
	case "studiolink-builtin":
		return "builtin"
	default:
		return oneLine(engineName)
	}
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
