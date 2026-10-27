package export

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SourceScreen marks a screen-share deliverable (ExportSource.Source).
const SourceScreen = "screen"

// SourcePhone marks a phone camera's deliverables (role subsource), exported
// under its participant as an extra angle [SL-P1-9].
const SourcePhone = "phone"

// ExportSource represents one aligned media source track.
type ExportSource struct {
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	ID          string `json:"id"`
	Participant string `json:"participant"`
	// ParticipantID is the session participant who owns the source (additive, SL-P4-1).
	ParticipantID string  `json:"participantId,omitempty"`
	Label         string  `json:"label"`
	Kind          string  `json:"kind"`             // "video", "audio"
	Source        string  `json:"source,omitempty"` // "screen" for a screen share, "phone" for a phone camera; empty for camera/mic
	FileRelPath   string  `json:"file"`
	FileAbsPath   string  `json:"-"`
	OffsetSec     float64 `json:"offsetSec"`
	DurationSec   float64 `json:"durationSec"`
	DriftPpm      float64 `json:"driftPpm"`
	Method        string  `json:"method"`
	ResidualMs    float64 `json:"residualMs"`
	// Channels is an audio deliverable's channel count (probed; 0 unknown).
	Channels int `json:"channels,omitempty"`
	// Acoustic is the independent check of this source's placement against
	// the host-received microphone reference (refrec) [SL-P0-10]; nil for
	// screen shares.
	Acoustic *AcousticCheck `json:"acoustic,omitempty"`
	// MicProcessing is how the browser processed this microphone while
	// recording (audio from a participant's mic only) [SL-P1-10].
	MicProcessing *MicProcessing `json:"micProcessing,omitempty"`
}

// AcousticCheck is the manifest record of align.VerifyAcoustic for one
// source (see docs/ARCHITECTURE.md §6). Status: verified (the clock offset
// agrees within uncertaintyMs), corrected (offset moved by appliedMs),
// disagrees (confident but beyond the correction window: clock kept),
// unmeasurable (too little common speech or inconsistent windows),
// no-reference (the host has no received copy of this person's mic).
// errorMs is measured minus modelled reference delay at the middle of the
// take; driftPpm the residual audio-clock drift (reported, not applied).
// Anchor is the track that was measured; other tracks of the person carry
// the result with appliedMs 0 unless they were placed through the anchor.
type AcousticCheck struct {
	Method          string  `json:"method"`
	Status          string  `json:"status"`
	Anchor          string  `json:"anchor,omitempty"`
	ErrorMs         float64 `json:"errorMs"`
	DelayMs         float64 `json:"delayMs"`
	ExpectedDelayMs float64 `json:"expectedDelayMs"`
	SpreadMs        float64 `json:"spreadMs"`
	DriftPpm        float64 `json:"driftPpm"`
	UncertaintyMs   float64 `json:"uncertaintyMs"`
	AppliedMs       float64 `json:"appliedMs"`
	Windows         int     `json:"windows"`
	UsedWindows     int     `json:"usedWindows"`
}

// MicProcessing tells the editor whether an isolated microphone track is raw
// (no browser processing; best for editing, needs headphones) or clean (echo
// cancellation + noise suppression, no automatic gain; used without
// headphones so other voices don't bleed in). Mode is the participant's
// setting (auto/raw/clean), Resolved what it became, Reason why; the
// booleans are the recording track's measured browser settings.
type MicProcessing struct {
	Mode             string `json:"mode"`
	Resolved         string `json:"resolved"`
	Reason           string `json:"reason,omitempty"`
	Headphones       bool   `json:"headphones"`
	EchoCancellation bool   `json:"echoCancellation"`
	NoiseSuppression bool   `json:"noiseSuppression"`
	AutoGainControl  bool   `json:"autoGainControl"`
	FollowsCall      bool   `json:"followsCall,omitempty"`
}

// SanitizeMicProcessing keeps a client-reported value only when its enums are
// known (it comes from a participant's sidecar).
func SanitizeMicProcessing(m *MicProcessing) *MicProcessing {
	if m == nil || (m.Mode != "auto" && m.Mode != "raw" && m.Mode != "clean") || (m.Resolved != "raw" && m.Resolved != "clean") {
		return nil
	}
	out := *m
	switch out.Reason {
	case "participant", "session", "headphones", "no_headphones":
	default:
		out.Reason = ""
	}
	return &out
}

// Manifest represents manifest.json.
type Manifest struct {
	Schema         string         `json:"schema"`
	Session        SessionMeta    `json:"session"`
	Take           int            `json:"take"`
	StartWallClock string         `json:"startWallClock"`
	FrameRate      string         `json:"frameRate"`
	SampleRate     int            `json:"sampleRate"`
	Sources        []ExportSource `json:"sources"`
	// Participants are the people recorded in this take, with the lower-third
	// text the host or producer set for them [SL-P5-2], so an editor can add
	// titles. Recordings themselves stay clean (nothing is burned in).
	Participants []ManifestParticipant `json:"participants,omitempty"`
}

// ManifestParticipant is one recorded person. LowerThirdName is the name to
// show in a lower third (the display name unless overridden); Title is the
// second line ("Founder, X"), empty when none was set.
type ManifestParticipant struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Role           string `json:"role"`
	LowerThirdName string `json:"lowerThirdName"`
	Title          string `json:"title"`
}

type SessionMeta struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// RationalFrameRate encapsulates precise frame rate geometry for NLE timelines.
type RationalFrameRate struct {
	Numerator   int64   `json:"numerator"`
	Denominator int64   `json:"denominator"`
	Timebase    int     `json:"timebase"`
	IsNTSC      bool    `json:"is_ntsc"`
	FrameDurStr string  `json:"frame_dur_str"`
	FormatName  string  `json:"format_name"`
	FPSFloat    float64 `json:"fps_float"`
}

// ResolveFrameRate maps a floating-point frame rate to exact standard NLE rational fractions.
func ResolveFrameRate(fps float64) RationalFrameRate {
	if fps <= 0 {
		fps = 30.0
	}

	// 29.97 fps (NTSC 30000/1001)
	if math.Abs(fps-29.97) < 0.015 || math.Abs(fps-(30000.0/1001.0)) < 0.001 {
		return RationalFrameRate{
			Numerator:   30000,
			Denominator: 1001,
			Timebase:    30,
			IsNTSC:      true,
			FrameDurStr: "1001/30000s",
			FormatName:  "FFVideoFormat1080p2997",
			FPSFloat:    30000.0 / 1001.0,
		}
	}

	// 23.976 / 23.98 fps (NTSC 24000/1001)
	if math.Abs(fps-23.976) < 0.015 || math.Abs(fps-23.98) < 0.015 || math.Abs(fps-(24000.0/1001.0)) < 0.001 {
		return RationalFrameRate{
			Numerator:   24000,
			Denominator: 1001,
			Timebase:    24,
			IsNTSC:      true,
			FrameDurStr: "1001/24000s",
			FormatName:  "FFVideoFormat1080p2398",
			FPSFloat:    24000.0 / 1001.0,
		}
	}

	// 59.94 fps (NTSC 60000/1001)
	if math.Abs(fps-59.94) < 0.015 || math.Abs(fps-(60000.0/1001.0)) < 0.001 {
		return RationalFrameRate{
			Numerator:   60000,
			Denominator: 1001,
			Timebase:    60,
			IsNTSC:      true,
			FrameDurStr: "1001/60000s",
			FormatName:  "FFVideoFormat1080p5994",
			FPSFloat:    60000.0 / 1001.0,
		}
	}

	// Integer frame rates (24, 25, 30, 60, etc.)
	tb := int(math.Round(fps))
	if tb <= 0 {
		tb = 30
	}
	return RationalFrameRate{
		Numerator:   int64(tb),
		Denominator: 1,
		Timebase:    tb,
		IsNTSC:      false,
		FrameDurStr: fmt.Sprintf("1/%ds", tb),
		FormatName:  fmt.Sprintf("FFVideoFormat1080p%d", tb),
		FPSFloat:    float64(tb),
	}
}

// WriteManifest writes the manifest.json file.
func WriteManifest(outDir string, sessID, title string, takeNum int, wallClock time.Time, fps float64, sources []ExportSource, participants ...ManifestParticipant) error {
	rfr := ResolveFrameRate(fps)
	frameRateStr := fmt.Sprintf("%d/%d", rfr.Numerator, rfr.Denominator)

	m := Manifest{
		Schema: "studiolink.take/1",
		Session: SessionMeta{
			ID:    sessID,
			Title: title,
		},
		Take:           takeNum,
		StartWallClock: wallClock.Format(time.RFC3339Nano),
		FrameRate:      frameRateStr,
		SampleRate:     48000,
		Sources:        sources,
		Participants:   participants,
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0644)
}

// Timeline placement shared by both editor formats [SL-P0-10].
//
// Video is conformed to CFR, so a camera's offset is rounded to the nearest
// frame. A microphone keeps its sub-frame placement where the format allows
// it: its clip starts on the first whole frame at or after the true offset
// and trims the fraction of a frame from the head of the file (FCPXML
// `start` in 1/48000 s). FCP7 XML only counts frames, so there the
// microphone is rounded to the nearest frame (error up to half a frame).
// Durations never exceed the media (floor, not round). A negative offset
// (a source that began before the take's start mark) moves the whole
// timeline later by whole frames, keeping every relative offset intact.

// placement is a source's edit position in timeline frames.
type placement struct {
	offFrames int64 // timeline offset
	durFrames int64 // edit duration
	// headSamples (FCPXML audio only) is the 48 kHz head trim that puts the
	// first kept sample exactly at its sub-frame position.
	headSamples int64
}

const frameEps = 1e-6

func (p placement) end() int64 { return p.offFrames + p.durFrames }

// originFrames is how many frames the timeline starts before the take's
// start mark, so that no source has a negative position.
func originFrames(sources []ExportSource, fps float64) int64 {
	var origin int64
	for _, s := range sources {
		if s.OffsetSec < 0 {
			origin = max(origin, int64(math.Ceil(-s.OffsetSec*fps-frameEps)))
		}
	}
	return origin
}

func placeVideo(s ExportSource, fps float64, origin int64) placement {
	return placement{
		offFrames: origin + int64(math.Round(s.OffsetSec*fps)),
		durFrames: int64(math.Floor(s.DurationSec*fps + frameEps)),
	}
}

// placeAudioSubframe is the FCPXML audio placement (sample-accurate start).
func placeAudioSubframe(s ExportSource, fps float64, origin int64) placement {
	at := float64(origin)/fps + s.OffsetSec // seconds on the timeline
	off := int64(math.Ceil(at*fps - frameEps))
	head := int64(math.Round((float64(off)/fps - at) * 48000))
	if head < 0 {
		head = 0
	}
	dur := int64(math.Floor((s.DurationSec-float64(head)/48000)*fps + frameEps))
	return placement{offFrames: off, durFrames: max(dur, 0), headSamples: head}
}

// placeAudioFrames is the FCP7 XML audio placement (nearest frame).
func placeAudioFrames(s ExportSource, fps float64, origin int64) placement {
	return placement{
		offFrames: origin + int64(math.Round(s.OffsetSec*fps)),
		durFrames: int64(math.Floor(s.DurationSec*fps + frameEps)),
	}
}

// GenerateFCPXML creates an FCPXML 1.10 multicam XML with precise rational timing.
// The structure follows Apple's FCPXML 1.10 DTD (checked element by element
// in fcpxml_dtd_test.go) and the Resolve 21.0.4 import constraints recorded
// in DECISIONS (2026-10-16).
func GenerateFCPXML(outDir string, title string, takeNum int, fps float64, sources []ExportSource) error {
	rfr := ResolveFrameRate(fps)
	fpsF := rfr.FPSFloat
	rat := func(frames int64) string { // frames as rational seconds
		if frames == 0 {
			return "0s"
		}
		return fmt.Sprintf("%d/%ds", frames*rfr.Denominator, rfr.Numerator)
	}
	samples := func(n int64) string {
		if n == 0 {
			return "0s"
		}
		return fmt.Sprintf("%d/48000s", n)
	}
	origin := originFrames(sources, fpsF)
	place := make([]placement, len(sources))
	var mcDurFrames int64
	for i, src := range sources {
		if src.Kind == "video" {
			place[i] = placeVideo(src, fpsF, origin)
		} else {
			place[i] = placeAudioSubframe(src, fpsF, origin)
		}
		mcDurFrames = max(mcDurFrames, place[i].end())
	}
	mcDurStr := rat(mcDurFrames)

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<!DOCTYPE fcpxml>` + "\n")
	sb.WriteString(`<fcpxml version="1.10">` + "\n")
	sb.WriteString("  <resources>\n")
	sb.WriteString(fmt.Sprintf(`    <format id="r1" name="%s" frameDuration="%s" width="1920" height="1080"/>`+"\n",
		rfr.FormatName, rfr.FrameDurStr))

	// Keep each asset's native geometry; the editing canvas remains 1080p.
	videoFormats := make(map[string]string)
	dimensions := map[[2]int]string{{1920, 1080}: "r1"}
	for _, src := range sources {
		if src.Kind != "video" {
			continue
		}
		w, h := src.Width, src.Height
		if w <= 0 || h <= 0 {
			w, h = 1920, 1080
		}
		key := [2]int{w, h}
		id, exists := dimensions[key]
		if !exists {
			id = fmt.Sprintf("videoformat%d", len(dimensions))
			dimensions[key] = id
			sb.WriteString(fmt.Sprintf(`    <format id="%s" frameDuration="%s" width="%d" height="%d"/>`+"\n", id, rfr.FrameDurStr, w, h))
		}
		videoFormats[src.ID] = id
	}

	// Media assets. Conformed camera files are silent (-an); microphones are
	// 48 kHz PCM WAV with their probed channel count.
	assetIdx := 2
	assetMap := make(map[string]string)
	for _, src := range sources {
		assetID := fmt.Sprintf("r%d", assetIdx)
		assetMap[src.ID] = assetID
		var attrs string
		if src.Kind == "video" {
			attrs = fmt.Sprintf(`duration="%s" hasVideo="1" hasAudio="0" format="%s" videoSources="1"`,
				rat(int64(math.Floor(src.DurationSec*fpsF+frameEps))), videoFormats[src.ID])
		} else {
			channels := ""
			if src.Channels > 0 {
				channels = fmt.Sprintf(` audioChannels="%d"`, src.Channels)
			}
			attrs = fmt.Sprintf(`duration="%s" hasVideo="0" hasAudio="1" audioSources="1"%s audioRate="48000"`,
				samples(int64(math.Floor(src.DurationSec*48000+frameEps))), channels)
		}
		sb.WriteString(fmt.Sprintf(`    <asset id="%s" name="%s" start="0s" %s><media-rep kind="original-media" src="%s"/></asset>`+"\n",
			assetID, xmlEscape(src.Label), attrs, xmlEscape(mediaURI(src))))
		assetIdx++
	}

	var videos, microphones []int
	for i, src := range sources {
		if src.Kind == "video" {
			videos = append(videos, i)
		} else {
			microphones = append(microphones, i)
		}
	}
	writeVideoClip := func(i int, indent string, offFrames int64) {
		src := sources[i]
		sb.WriteString(fmt.Sprintf(`%s<asset-clip name="%s" ref="%s" offset="%s" duration="%s" start="0s" format="%s" tcFormat="NDF"/>`+"\n",
			indent, xmlEscape(src.Label), assetMap[src.ID], rat(offFrames), rat(place[i].durFrames), videoFormats[src.ID]))
	}
	// Connected microphones: lane below the primary storyline, dialogue role
	// (a screen share's system audio is effects), sample-accurate head trim.
	writeAudioClip := func(i int, indent string, lane int) {
		src := sources[i]
		role := "dialogue"
		if src.Source == SourceScreen {
			role = "effects"
		}
		sb.WriteString(fmt.Sprintf(`%s<asset-clip name="%s" ref="%s" lane="%d" offset="%s" duration="%s" start="%s" audioRole="%s"/>`+"\n",
			indent, xmlEscape(src.Label), assetMap[src.ID], lane, rat(place[i].offFrames), rat(place[i].durFrames), samples(place[i].headSamples), role))
	}

	// Keep camera switching independent of the isolated microphones. Resolve
	// imports only one active multicam audio angle, and peels connected audio
	// out of composite angles. Put every microphone on the project timeline.
	mcMediaID := fmt.Sprintf("r%d", assetIdx)
	if len(videos) > 0 {
		sb.WriteString(fmt.Sprintf(`    <media id="%s" name="Take %d Multicam">`+"\n", mcMediaID, takeNum))
		// Optional in the DTD, but omitting either timecode attribute crashes
		// Resolve's native multicam importer.
		sb.WriteString(`      <multicam format="r1" tcStart="0s" tcFormat="NDF" renderFormat="original">` + "\n")
		for index, i := range videos {
			src := sources[i]
			angleName := "Camera"
			if src.Source == SourceScreen {
				angleName = "Screen"
			} else if src.Source == SourcePhone {
				angleName = "Phone"
			}
			sb.WriteString(fmt.Sprintf(`        <mc-angle name="%s %s" angleID="angle-%d">`+"\n", xmlEscape(src.Participant), angleName, index+1))
			// An angle is a storyline: items follow each other from 0s.
			if place[i].offFrames > 0 {
				sb.WriteString(fmt.Sprintf(`          <gap name="Gap" offset="0s" start="0s" duration="%s"/>`+"\n", rat(place[i].offFrames)))
			}
			// A direct primary video is required: Resolve imports a video
			// anchored to a full-duration gap as an empty timeline.
			writeVideoClip(i, "          ", place[i].offFrames)
			sb.WriteString("        </mc-angle>\n")
		}
		sb.WriteString("      </multicam>\n")
		sb.WriteString("    </media>\n")
	}

	sb.WriteString("  </resources>\n")

	// Library and project
	sb.WriteString("  <library>\n")
	sb.WriteString(fmt.Sprintf(`    <event name="%s">`+"\n", xmlEscape(title)))
	sb.WriteString(fmt.Sprintf(`      <project name="Take %d Synced Multicam">`+"\n", takeNum))
	sb.WriteString(fmt.Sprintf(`        <sequence format="r1" duration="%s" tcStart="0s" tcFormat="NDF" audioLayout="stereo" audioRate="48k">`+"\n", mcDurStr))
	sb.WriteString("          <spine>\n")
	if len(videos) > 0 {
		sb.WriteString(fmt.Sprintf(`            <mc-clip name="Take %d Multicam" ref="%s" offset="0s" start="0s" duration="%s" srcEnable="video">`+"\n", takeNum, mcMediaID, mcDurStr))
		sb.WriteString(`              <mc-source angleID="angle-1" srcEnable="video"/>` + "\n")
	} else {
		// Audio-only takes still need a full-duration primary storyline to
		// anchor all microphones without inventing a camera/multicam source.
		sb.WriteString(fmt.Sprintf(`            <gap name="Audio take" offset="0s" start="0s" duration="%s">`+"\n", mcDurStr))
	}
	for index, i := range microphones {
		writeAudioClip(i, "              ", -(index + 1))
	}
	if len(videos) > 0 {
		sb.WriteString("            </mc-clip>\n")
	} else {
		sb.WriteString("            </gap>\n")
	}

	sb.WriteString("          </spine>\n")
	sb.WriteString("        </sequence>\n")
	sb.WriteString("      </project>\n")
	sb.WriteString("    </event>\n")
	sb.WriteString("  </library>\n")
	sb.WriteString("</fcpxml>\n")

	return os.WriteFile(filepath.Join(outDir, fmt.Sprintf("take_%02d_multicam.fcpxml", takeNum)), []byte(sb.String()), 0644)
}

// xmemlPathURL is the FCP7 pathurl of a local file: file://localhost/ plus
// the percent-encoded absolute path, as Final Cut Pro 7 and Premiere write it.
func xmemlPathURL(abs string) string {
	return (&url.URL{Scheme: "file", Host: "localhost", Path: filepath.ToSlash(abs)}).String()
}

// GenerateXMEML creates an FCP7 XML / xmeml v4 sequence for Premiere Pro and
// DaVinci Resolve: one video track per camera, one audio track per
// microphone, each a single clip at its aligned start. Element order and
// required children follow the FCP7 XML interchange format as Premiere
// writes it (checked in xmeml_test.go).
func GenerateXMEML(outDir string, title string, takeNum int, fps float64, sources []ExportSource) error {
	rfr := ResolveFrameRate(fps)
	ntscStr := "FALSE"
	if rfr.IsNTSC {
		ntscStr = "TRUE"
	}
	rate := fmt.Sprintf("<rate><timebase>%d</timebase><ntsc>%s</ntsc></rate>", rfr.Timebase, ntscStr)
	origin := originFrames(sources, rfr.FPSFloat)
	place := make([]placement, len(sources))
	// Resolve uses the sequence duration to construct its tracks; omitting it
	// silently imports a zero-length timeline despite valid clip start/end values.
	var timelineFrames int64
	for i, src := range sources {
		if src.Kind == "video" {
			place[i] = placeVideo(src, rfr.FPSFloat, origin)
		} else {
			place[i] = placeAudioFrames(src, rfr.FPSFloat, origin)
		}
		timelineFrames = max(timelineFrames, place[i].end())
	}

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<!DOCTYPE xmeml>` + "\n")
	sb.WriteString(`<xmeml version="4">` + "\n")
	sb.WriteString(`  <sequence id="sequence-1">` + "\n")
	sb.WriteString(fmt.Sprintf("    <duration>%d</duration>\n", timelineFrames))
	sb.WriteString("    " + rate + "\n")
	sb.WriteString(fmt.Sprintf("    <name>Take %d - %s</name>\n", takeNum, xmlEscape(title)))
	sb.WriteString("    <media>\n")
	sb.WriteString("      <video>\n")
	sb.WriteString("        <format><samplecharacteristics>" + rate + "<width>1920</width><height>1080</height><anamorphic>FALSE</anamorphic><pixelaspectratio>square</pixelaspectratio><fielddominance>none</fielddominance></samplecharacteristics></format>\n")

	clip := func(kind string, i int, src ExportSource) {
		pl := place[i]
		id := fmt.Sprintf("clipitem-%s-%d", kind, i+1)
		sb.WriteString("        <track>\n")
		sb.WriteString(fmt.Sprintf("          <clipitem id=\"%s\">\n", id))
		sb.WriteString(fmt.Sprintf("            <name>%s</name>\n", xmlEscape(src.Label)))
		sb.WriteString("            <enabled>TRUE</enabled>\n")
		sb.WriteString(fmt.Sprintf("            <duration>%d</duration>%s\n", pl.durFrames, rate))
		sb.WriteString(fmt.Sprintf("            <start>%d</start>\n", pl.offFrames))
		sb.WriteString(fmt.Sprintf("            <end>%d</end>\n", pl.end()))
		sb.WriteString("            <in>0</in>\n")
		sb.WriteString(fmt.Sprintf("            <out>%d</out>\n", pl.durFrames))
		fileID := "file-" + src.ID
		if kind == "a" {
			fileID = "file-audio-" + src.ID
		}
		sb.WriteString(fmt.Sprintf("            <file id=\"%s\">\n", xmlEscape(fileID)))
		sb.WriteString(fmt.Sprintf("              <name>%s</name>\n", xmlEscape(filepath.Base(src.FileAbsPath))))
		sb.WriteString(fmt.Sprintf("              <pathurl>%s</pathurl>\n", xmlEscape(xmemlPathURL(src.FileAbsPath))))
		sb.WriteString("              " + rate + "\n")
		sb.WriteString(fmt.Sprintf("              <duration>%d</duration>\n", pl.durFrames))
		sb.WriteString("              <timecode>" + rate + "<string>00:00:00:00</string><frame>0</frame><displayformat>NDF</displayformat></timecode>\n")
		if kind == "v" {
			w, h := src.Width, src.Height
			if w <= 0 || h <= 0 {
				w, h = 1920, 1080
			}
			sb.WriteString(fmt.Sprintf("              <media><video><samplecharacteristics>%s<width>%d</width><height>%d</height><anamorphic>FALSE</anamorphic><pixelaspectratio>square</pixelaspectratio><fielddominance>none</fielddominance></samplecharacteristics></video></media>\n", rate, w, h))
		} else {
			channels := src.Channels
			if channels <= 0 {
				channels = 2
			}
			sb.WriteString(fmt.Sprintf("              <media><audio><samplecharacteristics><depth>24</depth><samplerate>48000</samplerate></samplecharacteristics><channelcount>%d</channelcount></audio></media>\n", channels))
		}
		sb.WriteString("            </file>\n")
		if kind == "a" {
			// Every channel of the file plays on this one track.
			sb.WriteString("            <sourcetrack><mediatype>audio</mediatype><trackindex>1</trackindex></sourcetrack>\n")
		}
		sb.WriteString("          </clipitem>\n")
		sb.WriteString("        </track>\n")
	}
	for i, src := range sources {
		if src.Kind == "video" {
			clip("v", i, src)
		}
	}
	sb.WriteString("      </video>\n")
	sb.WriteString("      <audio>\n")
	for i, src := range sources {
		if src.Kind == "audio" {
			clip("a", i, src)
		}
	}
	sb.WriteString("      </audio>\n")
	sb.WriteString("    </media>\n")
	sb.WriteString("    <timecode>" + rate + "<string>00:00:00:00</string><frame>0</frame><displayformat>NDF</displayformat></timecode>\n")
	sb.WriteString("  </sequence>\n")
	sb.WriteString("</xmeml>\n")

	return os.WriteFile(filepath.Join(outDir, fmt.Sprintf("take_%02d_premiere.xml", takeNum)), []byte(sb.String()), 0644)
}

func xmlEscape(s string) string {
	var buf strings.Builder
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// Relative URLs remain valid when the complete editor package moves machines.
func mediaURI(src ExportSource) string {
	if src.FileRelPath != "" && filepath.IsLocal(src.FileRelPath) {
		return (&url.URL{Path: "./" + filepath.ToSlash(src.FileRelPath)}).String()
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(src.FileAbsPath)}).String()
}
