package export

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExports(t *testing.T) {
	tempDir := t.TempDir()

	sources := []ExportSource{
		{
			ID:          "s1",
			Participant: "Ben",
			Label:       "FaceTime Cam",
			Kind:        "video",
			FileRelPath: "video/s1.mov",
			FileAbsPath: "/Users/test/Movies/s1.mov",
			OffsetSec:   0.05,
			DurationSec: 120.0,
			DriftPpm:    12.0,
			Method:      "gcc-phat",
			ResidualMs:  0.3,
		},
		{
			ID:          "s2",
			Participant: "Guest",
			Label:       "Remote Cam",
			Kind:        "video",
			FileRelPath: "video/s2.mov",
			FileAbsPath: "/Users/test/Movies/s2.mov",
			OffsetSec:   0.12,
			DurationSec: 119.8,
			DriftPpm:    -5.0,
			Method:      "gcc-phat",
			ResidualMs:  0.4,
		},
	}

	// 1. Manifest
	err := WriteManifest(tempDir, "sess_1", "Test Episode", 1, time.Now().UTC(), 30, sources)
	if err != nil {
		t.Fatalf("WriteManifest failed: %v", err)
	}
	manifestPath := filepath.Join(tempDir, "manifest.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest.json missing: %v", err)
	}

	// 2. FCPXML
	err = GenerateFCPXML(tempDir, "Test Episode", 1, 30, sources)
	if err != nil {
		t.Fatalf("GenerateFCPXML failed: %v", err)
	}
	fcpxmlPath := filepath.Join(tempDir, "take_01_multicam.fcpxml")
	data, err := os.ReadFile(fcpxmlPath)
	if err != nil {
		t.Fatalf("fcpxml missing: %v", err)
	}
	// Verify XML is well-formed
	var xmlObj interface{}
	if err := xml.Unmarshal(data, &xmlObj); err != nil {
		t.Fatalf("FCPXML invalid XML: %v", err)
	}

	// 3. XMEML
	err = GenerateXMEML(tempDir, "Test Episode", 1, 30, sources)
	if err != nil {
		t.Fatalf("GenerateXMEML failed: %v", err)
	}
	xmemlPath := filepath.Join(tempDir, "take_01_premiere.xml")
	xData, err := os.ReadFile(xmemlPath)
	if err != nil {
		t.Fatalf("xmeml missing: %v", err)
	}
	if err := xml.Unmarshal(xData, &xmlObj); err != nil {
		t.Fatalf("xmeml invalid XML: %v", err)
	}
}

func TestFractionalNTSCExports(t *testing.T) {
	tempDir := t.TempDir()

	sources := []ExportSource{
		{
			ID:          "s1",
			Participant: "Host",
			Label:       "Main Camera",
			Kind:        "video",
			FileRelPath: "video/s1.mov",
			FileAbsPath: "/Users/test/Movies/s1.mov",
			OffsetSec:   0.033366,
			DurationSec: 3600.0,
			DriftPpm:    0.0,
			Method:      "gcc-phat",
			ResidualMs:  0.2,
		},
	}

	// Test 29.97 NTSC
	if err := GenerateFCPXML(tempDir, "NTSC Episode", 2, 29.97, sources); err != nil {
		t.Fatalf("GenerateFCPXML (29.97) failed: %v", err)
	}
	fcpxml2997, err := os.ReadFile(filepath.Join(tempDir, "take_02_multicam.fcpxml"))
	if err != nil {
		t.Fatalf("read fcpxml 29.97 failed: %v", err)
	}
	content := string(fcpxml2997)
	if !strings.Contains(content, "FFVideoFormat1080p2997") {
		t.Errorf("Expected FFVideoFormat1080p2997 in FCPXML, got: %s", content)
	}
	if !strings.Contains(content, `frameDuration="1001/30000s"`) {
		t.Errorf("Expected frameDuration=1001/30000s, got: %s", content)
	}

	if err := GenerateXMEML(tempDir, "NTSC Episode", 2, 29.97, sources); err != nil {
		t.Fatalf("GenerateXMEML (29.97) failed: %v", err)
	}
	xmeml2997, err := os.ReadFile(filepath.Join(tempDir, "take_02_premiere.xml"))
	if err != nil {
		t.Fatalf("read xmeml 29.97 failed: %v", err)
	}
	xContent := string(xmeml2997)
	if !strings.Contains(xContent, "<timebase>30</timebase>") || !strings.Contains(xContent, "<ntsc>TRUE</ntsc>") {
		t.Errorf("Expected timebase 30 and ntsc TRUE in XMEML, got: %s", xContent)
	}

	// Test 23.976 NTSC
	if err := GenerateFCPXML(tempDir, "Cinema Episode", 3, 23.976, sources); err != nil {
		t.Fatalf("GenerateFCPXML (23.976) failed: %v", err)
	}
	fcpxml2398, err := os.ReadFile(filepath.Join(tempDir, "take_03_multicam.fcpxml"))
	if err != nil {
		t.Fatalf("read fcpxml 23.976 failed: %v", err)
	}
	c2398 := string(fcpxml2398)
	if !strings.Contains(c2398, "FFVideoFormat1080p2398") || !strings.Contains(c2398, `frameDuration="1001/24000s"`) {
		t.Errorf("Expected 23.976 rational formatting, got: %s", c2398)
	}

	// Test Manifest
	if err := WriteManifest(tempDir, "sess_ntsc", "NTSC Episode", 2, time.Now().UTC(), 29.97, sources); err != nil {
		t.Fatalf("WriteManifest failed: %v", err)
	}
	manifestBytes, _ := os.ReadFile(filepath.Join(tempDir, "manifest.json"))
	if !strings.Contains(string(manifestBytes), `"frameRate": "30000/1001"`) {
		t.Errorf("Expected frameRate 30000/1001 in manifest, got: %s", string(manifestBytes))
	}
}

func TestPortableMulticamUsesActualDurationAndIsolatedAudio(t *testing.T) {
	dir := t.TempDir()
	sources := []ExportSource{
		{ID: "v1", Participant: "One", Label: "Cam & one", Kind: "video", FileRelPath: "video/one & two.mov", FileAbsPath: "/private/host/video.mov", OffsetSec: 0.1, DurationSec: 10},
		{ID: "a1", Participant: "One", Label: "Mic", Kind: "audio", FileRelPath: "audio/one.wav", FileAbsPath: "/private/host/audio.wav", OffsetSec: 0.1, DurationSec: 10},
		{ID: "v2", Participant: "Two", Label: "Cam", Kind: "video", FileRelPath: "video/two.mov", OffsetSec: 0.2, DurationSec: 11},
	}
	if err := GenerateFCPXML(dir, "Portable", 1, 30, sources); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
	s := string(data)
	if strings.Contains(s, "/private/host") || strings.Contains(s, ` src="file://`) {
		t.Fatal("host paths leaked into portable timeline")
	}
	if !strings.Contains(s, `<media-rep kind="original-media" src="./video/one%20&amp;%20two.mov"`) {
		t.Fatal("missing escaped portable media-rep")
	}
	if strings.Count(s, "<mc-angle ") != 2 {
		t.Fatal("only cameras belong in multicam angles")
	}
	if !strings.Contains(s, `duration="336/30s"`) || strings.Contains(s, "108000/30s") {
		t.Fatal("timeline duration must follow final media end")
	}
	var doc interface{}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
}

// These fields are optional in the DTD, but Resolve crashes on a multicam
// resource without them. Check the resource itself, not just the sequence.
func TestMulticamHasExplicitTimecodeForResolve(t *testing.T) {
	for _, fps := range []float64{30, 29.97, 23.976} {
		t.Run(ResolveFrameRate(fps).FormatName, func(t *testing.T) {
			dir := t.TempDir()
			if err := GenerateFCPXML(dir, "Synthetic", 1, fps, []ExportSource{{ID: "v", Kind: "video", FileRelPath: "video/camera.mov", DurationSec: 2}}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Multicam struct {
					Start  string `xml:"tcStart,attr"`
					Format string `xml:"tcFormat,attr"`
				} `xml:"resources>media>multicam"`
			}
			if err := xml.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Multicam.Start != "0s" || doc.Multicam.Format != "NDF" {
				t.Fatalf("multicam timecode origin=%q format=%q; Resolve requires both", doc.Multicam.Start, doc.Multicam.Format)
			}
		})
	}
}

func TestMulticamEditSelectsAnExistingAngle(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFCPXML(dir, "Synthetic", 1, 30, []ExportSource{{ID: "camera", Kind: "video", Participant: "One", DurationSec: 2}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Angle struct {
			ID string `xml:"angleID,attr"`
		} `xml:"resources>media>multicam>mc-angle"`
		Edit struct {
			Start  string `xml:"start,attr"`
			Source struct {
				ID     string `xml:"angleID,attr"`
				Enable string `xml:"srcEnable,attr"`
			} `xml:"mc-source"`
		} `xml:"library>event>project>sequence>spine>mc-clip"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Edit.Start != "0s" || doc.Edit.Source.ID != doc.Angle.ID || doc.Edit.Source.Enable != "video" {
		t.Fatal("multicam edit must explicitly select an existing video-only angle")
	}
}

type multicamPositionClip struct {
	Ref      string `xml:"ref,attr"`
	Lane     string `xml:"lane,attr"`
	Offset   string `xml:"offset,attr"`
	Start    string `xml:"start,attr"`
	Duration string `xml:"duration,attr"`
}

type multicamPositionDocument struct {
	Angles []struct {
		Clips []multicamPositionClip `xml:"asset-clip"`
	} `xml:"resources>media>multicam>mc-angle"`
	Sequence struct {
		Duration string `xml:"duration,attr"`
		Edit     struct {
			Enable  string `xml:"srcEnable,attr"`
			Sources []struct {
				Enable string `xml:"srcEnable,attr"`
			} `xml:"mc-source"`
			Clips []multicamPositionClip `xml:"asset-clip"`
		} `xml:"spine>mc-clip"`
		Gap struct {
			Duration string                 `xml:"duration,attr"`
			Clips    []multicamPositionClip `xml:"asset-clip"`
		} `xml:"spine>gap"`
	} `xml:"library>event>project>sequence"`
}

func TestMulticamKeepsEveryMicrophoneOutsideCameraSwitching(t *testing.T) {
	dir := t.TempDir()
	sources := []ExportSource{
		{ID: "mic1", Participant: "One", Kind: "audio", OffsetSec: 2.0 / 30, DurationSec: 630.0 / 30},
		{ID: "camera1", Participant: "One", Kind: "video", OffsetSec: 3.0 / 30, DurationSec: 628.0 / 30},
		{ID: "camera2", Participant: "Two", Kind: "video", OffsetSec: 4.0 / 30, DurationSec: 600.0 / 30},
		{ID: "mic2", Participant: "Two", Kind: "audio", OffsetSec: 1.0 / 30, DurationSec: 620.0 / 30},
	}
	if err := GenerateFCPXML(dir, "Synthetic", 1, 30, sources); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc multicamPositionDocument
	if err = xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Angles) != 2 {
		t.Fatal("exactly two camera angles required")
	}
	for i, want := range []struct{ ref, offset, duration string }{{"r3", "3/30s", "628/30s"}, {"r4", "4/30s", "600/30s"}} {
		clips := doc.Angles[i].Clips
		if len(clips) != 1 || clips[0].Ref != want.ref || clips[0].Offset != want.offset || clips[0].Duration != want.duration || clips[0].Lane != "" {
			t.Fatalf("camera %d must remain a direct primary source at its own position", i+1)
		}
	}
	edit := doc.Sequence.Edit
	if edit.Enable != "video" || len(edit.Sources) != 1 || edit.Sources[0].Enable != "video" {
		t.Fatal("camera switching must select video only")
	}
	if len(edit.Clips) != 2 {
		t.Fatal("both isolated microphones must remain connected outside the camera angles")
	}
	for i, want := range []struct{ ref, offset, duration, lane string }{{"r2", "2/30s", "630/30s", "-1"}, {"r5", "1/30s", "620/30s", "-2"}} {
		clip := edit.Clips[i]
		if clip.Ref != want.ref || clip.Offset != want.offset || clip.Duration != want.duration || clip.Lane != want.lane || clip.Start != "0s" {
			t.Fatalf("microphone %d prefix, tail or independent placement changed: %+v", i+1, clip)
		}
	}
	if doc.Sequence.Duration != "632/30s" {
		t.Fatal("sequence must include the longer microphone tail without an invented frame")
	}
}

func TestAudioOnlyFCPXMLKeepsEveryMicrophoneOnTimeline(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateFCPXML(dir, "Synthetic audio", 1, 30, []ExportSource{
		{ID: "mic1", Kind: "audio", OffsetSec: .1, DurationSec: 2},
		{ID: "mic2", Kind: "audio", OffsetSec: .2, DurationSec: 3},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "take_01_multicam.fcpxml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc multicamPositionDocument
	if err = xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Angles) != 0 || strings.Contains(string(data), "<mc-clip ") || strings.Contains(string(data), "<multicam ") {
		t.Fatal("audio-only take must not invent camera or multicam resources")
	}
	gap := doc.Sequence.Gap
	if gap.Duration != "96/30s" || doc.Sequence.Duration != gap.Duration || len(gap.Clips) != 2 {
		t.Fatal("audio-only primary gap must cover every microphone in full")
	}
	if gap.Clips[0].Offset != "3/30s" || gap.Clips[0].Duration != "60/30s" || gap.Clips[0].Lane != "-1" || gap.Clips[1].Offset != "6/30s" || gap.Clips[1].Duration != "90/30s" || gap.Clips[1].Lane != "-2" {
		t.Fatal("audio-only source placement changed")
	}
}

func TestXMEMLDoesNotInventAudioForSilentCameraDeliverables(t *testing.T) {
	dir := t.TempDir()
	sources := []ExportSource{{ID: "v", Kind: "video", FileAbsPath: "/cam.mov", DurationSec: 2}, {ID: "a", Kind: "audio", FileAbsPath: "/mic.wav", DurationSec: 2}}
	if err := GenerateXMEML(dir, "Synthetic", 1, 30, sources); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "take_01_premiere.xml"))
	s := string(b)
	if strings.Count(s, `id="clipitem-a-`) != 1 {
		t.Fatal("silent conformed video must not create duplicate audio clips")
	}
}

// Resolve imported a zero-length timeline when sequence duration was omitted,
// even though the XML contained nonempty clips. Exercise unequal source ends.
func TestXMEMLSequenceIncludesFullTimelineDuration(t *testing.T) {
	dir := t.TempDir()
	sources := []ExportSource{
		{ID: "camera", Kind: "video", Label: "Camera", FileAbsPath: "/tmp/Video & Audio/camera.mov", OffsetSec: 0.1, DurationSec: 20.933333333},
		{ID: "mic", Kind: "audio", Label: "Microphone", FileAbsPath: "/tmp/mic.wav", OffsetSec: 0.066666667, DurationSec: 20.966666667},
	}
	if err := GenerateXMEML(dir, "Resolve test", 1, 30, sources); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "take_01_premiere.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sequence struct {
			Duration int `xml:"duration"`
			Media    struct {
				Video struct {
					Tracks []struct {
						Clip struct {
							End int `xml:"end"`
						} `xml:"clipitem"`
					} `xml:"track"`
				} `xml:"video"`
				Audio struct {
					Tracks []struct {
						Clip struct {
							End int `xml:"end"`
						} `xml:"clipitem"`
					} `xml:"track"`
				} `xml:"audio"`
			} `xml:"media"`
		} `xml:"sequence"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Sequence.Duration != 631 {
		t.Fatalf("timeline duration=%d; Resolve requires the complete 631-frame sequence", doc.Sequence.Duration)
	}
	if strings.Count(string(data), "<duration>629</duration><rate>") != 1 || !strings.Contains(string(data), "<duration>628</duration><rate>") {
		t.Fatal("each clip needs its own duration and rate for Resolve import")
	}
	if !strings.Contains(string(data), "Video%20&amp;%20Audio/camera.mov") {
		t.Fatal("media URL must be URL-encoded and XML-escaped")
	}
}

func TestXMEMLMediaURLsAndFileNamesAreValid(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateXMEML(dir, "Escaped", 1, 30, []ExportSource{{ID: "video", Kind: "video", FileAbsPath: "/tmp/Video & Audio/camera & film.mov", DurationSec: 2}, {ID: "audio", Kind: "audio", FileAbsPath: "/tmp/Video & Audio/mic & audio.wav", DurationSec: 2}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "take_01_premiere.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err = xml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("participant-derived filenames broke XML: %v", err)
	}
	s := string(b)
	if strings.Contains(s, "file://file:") || !strings.Contains(s, "file://localhost/tmp/Video%20&amp;%20Audio/camera%20&amp;%20film.mov") {
		t.Fatal("media path must be one correctly encoded file URI")
	}
	if !strings.Contains(s, "<name>camera &amp; film.mov</name>") {
		t.Fatal("file name must be XML escaped")
	}
}
