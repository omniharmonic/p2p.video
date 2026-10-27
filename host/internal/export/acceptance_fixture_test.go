package export

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWriteResolveAcceptanceFixture writes the timelines and manifest of a
// synthetic editor package for scripts/resolve-acceptance.py, which then
// renders matching media with ffmpeg and imports both timelines into a
// throwaway Resolve project [SL-P0-10]. Skipped unless
// STUDIOLINK_ACCEPTANCE_PACKAGE names an output directory.
//
// The package exercises what the editor import must preserve: a camera that
// started before the take mark (negative offset), sub-frame microphone
// offsets, a portrait phone angle, a late screen share with its own audio,
// a mono and a stereo microphone.
func TestWriteResolveAcceptanceFixture(t *testing.T) {
	dir := os.Getenv("STUDIOLINK_ACCEPTANCE_PACKAGE")
	if dir == "" {
		t.Skip("set STUDIOLINK_ACCEPTANCE_PACKAGE to write the fixture")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	sources := []ExportSource{
		{ID: "cam-ana", Participant: "Ana", Label: "Camera", Kind: "video", FileRelPath: "video/Ana_Camera.mov", OffsetSec: -0.0412, DurationSec: 20.0, Width: 1920, Height: 1080, Method: "sidecar-coarse"},
		{ID: "mic-ana", Participant: "Ana", Label: "Mic", Kind: "audio", FileRelPath: "audio/Ana_Mic.wav", OffsetSec: 0.0137, DurationSec: 19.95, Channels: 1, Method: "paired-xcorr"},
		{ID: "cam-bo", Participant: "Bo", Label: "Camera", Kind: "video", FileRelPath: "video/Bo_Camera.mov", OffsetSec: 0.533, DurationSec: 19.0, Width: 1280, Height: 720, Method: "sidecar-coarse"},
		{ID: "mic-bo", Participant: "Bo", Label: "Mic & Voice", Kind: "audio", FileRelPath: "audio/Bo_Mic.wav", OffsetSec: 0.5091, DurationSec: 19.1, Channels: 2, Method: "sidecar-coarse"},
		{ID: "phone-bo", Participant: "Bo", Label: "Phone Camera", Kind: "video", Source: SourcePhone, FileRelPath: "video/Bo_Phone.mov", OffsetSec: 4.2, DurationSec: 12.0, Width: 1080, Height: 1920, Method: "sidecar-coarse"},
		{ID: "scr-ana", Participant: "Ana", Label: "Screen", Kind: "video", Source: SourceScreen, FileRelPath: "video/Ana_Screen.mov", OffsetSec: 8.0, DurationSec: 6.0, Width: 1920, Height: 1080, Method: "sidecar-coarse"},
		{ID: "scr-ana-a", Participant: "Ana", Label: "Screen Audio", Kind: "audio", Source: SourceScreen, FileRelPath: "audio/Ana_Screen_audio.wav", OffsetSec: 8.0213, DurationSec: 5.98, Channels: 2, Method: "sidecar-coarse"},
	}
	for i := range sources {
		sources[i].FileAbsPath = filepath.Join(abs, sources[i].FileRelPath)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if err := WriteManifest(abs, "acceptance", "p2p.video acceptance", 1, when, 30, sources); err != nil {
		t.Fatal(err)
	}
	if err := GenerateFCPXML(abs, "p2p.video acceptance", 1, 30, sources); err != nil {
		t.Fatal(err)
	}
	if err := GenerateXMEML(abs, "p2p.video acceptance", 1, 30, sources); err != nil {
		t.Fatal(err)
	}
}
