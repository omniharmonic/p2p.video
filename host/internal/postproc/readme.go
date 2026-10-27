package postproc

import (
	"fmt"
	"sort"
	"strings"

	"github.com/omniharmonic/studiolink/host/internal/align"
	"github.com/omniharmonic/studiolink/host/internal/export"
)

// editorReadme is the package's README.txt. The sync paragraph reports what
// was actually measured for this take (manifest "acoustic") [SL-P0-10].
func editorReadme(sources []export.ExportSource) string {
	var b strings.Builder
	b.WriteString("p2p.video editor package\n\n")
	b.WriteString("1. Unzip the complete package before importing. Keep video/ and audio/ beside the XML files.\n")
	b.WriteString("2. Timelines: take_NN_multicam.fcpxml (FCPXML 1.10: one multicam clip for camera switching, every microphone connected below it) and take_NN_premiere.xml (FCP7 XML: one track per camera and per microphone). In DaVinci Resolve use File > Import > Timeline (earlier versions of both layouts imported natively in Resolve 21.0.4). The FCPXML passes Apple's FCPXML 1.10 DTD but is experimental until Final Cut Pro imports it; the FCP7 XML follows the structure Premiere Pro writes but has not been opened in Premiere. Camera switching controls video only; every isolated microphone stays on its own track. The FCPXML links media relative to this folder; the FCP7 XML keeps the host's absolute paths, so on another machine point the editor's Locate/Relink dialog at this package's audio/ and video/ folders.\n")
	b.WriteString("3. Sync. " + syncSummary(sources) + "\n")
	b.WriteString("4. Check the start and end of each angle against its microphone before cutting.\n\n")
	b.WriteString("Camera files are separate from normalized 48 kHz WAV microphone files. manifest.json records each file's offset, duration, alignment method and, per person, the acoustic check (status, errorMs, spreadMs, driftPpm). In the FCPXML a microphone keeps its sample-accurate offset; FCP7 XML positions are whole frames (within half a frame). Browser-source recordings and clock estimates are not a hardware-lossless guarantee.\n")
	return b.String()
}

// syncSummary describes, in plain words, how each person was aligned.
func syncSummary(sources []export.ExportSource) string {
	type row struct{ name, text string }
	seen := map[string]bool{}
	var rows []row
	for _, s := range sources {
		if s.Acoustic == nil || s.Source == export.SourceScreen || seen[s.Participant+s.Source] {
			continue
		}
		seen[s.Participant+s.Source] = true
		name := s.Participant
		if s.Source == export.SourcePhone {
			name += " (phone)"
		}
		a := s.Acoustic
		var text string
		switch a.Status {
		case align.AcousticVerified:
			text = fmt.Sprintf("verified acoustically: the clock placement agrees with the host's received copy of the microphone within %.0f ms (measured %+.1f ms, spread %.1f ms over %d windows)", a.UncertaintyMs, a.ErrorMs, a.SpreadMs, a.UsedWindows)
		case align.AcousticCorrected:
			text = fmt.Sprintf("corrected acoustically by %+.1f ms (the clock placement disagreed with the host's received copy of the microphone; spread %.1f ms over %d windows; model uncertainty ±%.0f ms)", a.AppliedMs, a.SpreadMs, a.UsedWindows, a.UncertaintyMs)
		case align.AcousticDisagrees:
			text = fmt.Sprintf("NOT verified: the acoustic check disagrees by %+.1f ms, beyond the ±%.0f ms it may correct; the clock placement was kept. Check this person's sync by ear", a.ErrorMs, align.DefaultAcousticConfig().MaxCorrectionMs)
		case "no-reference":
			text = "clock-based only (no host copy of this microphone, e.g. a solo record-link take)"
		default:
			text = "clock-based only (too little common speech to check acoustically)"
		}
		rows = append(rows, row{name, text})
	}
	if len(rows) == 0 {
		return "Alignment is clock-based (each device's clock fit against the host); no acoustic check was possible for this take."
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })
	var b strings.Builder
	b.WriteString("Every file is placed by its device's clock fit against the host clock; a participant's uncompressed microphone is matched to the audio of their own camera. Each person's placement is then checked against the host's received copy of their microphone (GCC-PHAT; accuracy limited by the modelled send-path delay, about ±20 ms across browsers):")
	for _, r := range rows {
		b.WriteString("\n   - " + r.name + ": " + r.text + ".")
	}
	return b.String()
}
